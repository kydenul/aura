// Package hmac 实现外层 HMAC 的 TC-HMAC-SHA256 算法，
// 与具体框架解耦，HTTP middleware 或任何业务代码都可复用同一个 Manager 实例（用法对齐 pkg/jwt）。
//
// 与 JWT（pkg/jwt）面向"真人用户登录态"不同，HMAC 验"信封"——表达"调用方是哪个 appid"，
// 并保证 body 完整性与防重放。调用方与本服务预先共享 app_id / secret。
//
// 该包只做"签名 / 验签"一件事；HTTP 头解析、nonce 去重、ctx 注入、错误响应等框架侧能力放在
// internal/interceptor/hmac_middleware.go（与 pkg/jwt ↔ interceptor/auth.go 的分层一致）。
//
// 规范请求串（Canonical Request，字段以 "\n" 拼接，顺序固定）：
//
//	TC-HMAC-SHA256
//	<HTTP_METHOD>
//	<URI_PATH>
//	<CANONICAL_QUERY_STRING>
//	<HEX(SHA256(RAW_BODY))>
//	<X-Auth-Timestamp>
//	<X-Auth-Nonce>
//
//	Signature = 大写HEX( HMAC_SHA256(secret, Canonical Request) )
//
// 注意：appid 不参与签名串，仅用于选取 secret；签名结果为大写 hex，放进
// X-Auth-Sign: TC-HMAC-SHA256 Credential=<appid>,Signature=<SIG>（独立头，
// 不占用 Authorization——把 Authorization: Bearer 留给 JWT，避免两层鉴权头冲突）。
//
// 防重放由两部分组成：① 时间窗（Skew，默认 300s）；② nonce 去重（见 NonceStore，
// 通常由中间件配合 Redis SETNX 实现）。本包只负责把 nonce 纳入签名串与窗口校验。
package hmac

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Scheme 签名方案标识，既是规范串首行，也是 Authorization 头前缀。
const Scheme = "TC-HMAC-SHA256"

// 签名鉴权约定的请求头
const (
	// HeaderAppID 调用方 appid，用于选取 secret（不参与签名串）。
	HeaderAppID = "X-Auth-AppId"
	// HeaderTimestamp 请求发起的 Unix 秒级时间戳（字符串）。
	HeaderTimestamp = "X-Auth-Timestamp"
	// HeaderNonce 调用方生成的一次性随机串，配合 NonceStore 去重防重放。
	HeaderNonce = "X-Auth-Nonce"
	// HeaderSignature 承载签名，值形如 "TC-HMAC-SHA256 Credential=<appid>,Signature=<SIG>"。
	// 独立头，不复用 Authorization（Authorization: Bearer 留给 JWT）。
	HeaderSignature = "X-Auth-Sign"
)

// DefaultSkew 是未显式配置 Skew 时使用的时间窗（与方案文档收紧后的值一致）。
const DefaultSkew = 300 * time.Second

// 组件对外暴露的错误，调用方可用 errors.Is 精确判断失败原因。
var (
	// ErrNoKeys 创建 Manager 时未提供任何有效的 app_id / secret。
	ErrNoKeys = errors.New("hmac: at least one app secret must be configured")
	// ErrMissingSignature 缺少必需的签名头（app_id / timestamp / nonce / signature）或时间戳格式非法。
	ErrMissingSignature = errors.New("hmac: missing required signature header")
	// ErrUnknownAppID appid 未在密钥表中登记。
	ErrUnknownAppID = errors.New("hmac: unknown app id")
	// ErrTimestampExpired 请求时间戳超出允许的时间窗（疑似重放或时钟漂移）。
	ErrTimestampExpired = errors.New("hmac: timestamp out of allowed window")
	// ErrInvalidSignature 重算签名与请求携带的签名不一致。
	ErrInvalidSignature = errors.New("hmac: signature mismatch")
)

// Config 是 Manager 的配置项。
type Config struct {
	// Keys app_id → secret 的密钥表，必填（至少一条）。空 key / 空 secret 的条目会被忽略。
	Keys map[string]string
	// Skew 允许的请求时间戳与服务端时钟的最大偏移；<=0 时使用 DefaultSkew。
	Skew time.Duration
}

// Params 一次验签 / 签名所需的全部输入。
type Params struct {
	Method    string
	Path      string
	RawQuery  string // 原始 URL query（r.URL.RawQuery），由 CanonicalQuery 规范化
	AppID     string
	Timestamp string
	Nonce     string
	Signature string // 已从 Authorization 头解出的大写 hex 签名
	Body      []byte
}

// Manager 负责签名校验，创建后并发安全（内部密钥表只读），可作为单例长期持有。
type Manager struct {
	keys map[string]string
	skew time.Duration
}

// NewManager 根据配置创建一个 Manager；密钥表为空时返回 ErrNoKeys。
func NewManager(cfg Config) (*Manager, error) {
	keys := make(map[string]string, len(cfg.Keys))
	for k, v := range cfg.Keys {
		k = strings.TrimSpace(k)
		if k == "" || v == "" {
			continue
		}
		keys[k] = v
	}
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}

	skew := cfg.Skew
	if skew <= 0 {
		skew = DefaultSkew
	}
	return &Manager{keys: keys, skew: skew}, nil
}

// Skew 返回允许的时间窗，供中间件设置 nonce 去重 TTL（建议 TTL = 2×Skew，
// 覆盖正负向偏移并留出过期精度余量，详见 NonceStore 注释）。
func (m *Manager) Skew() time.Duration { return m.skew }

// Verify 校验一次请求的 TC-HMAC-SHA256 签名。now 一般传 time.Now()（参数化便于单测）。
// 成功返回通过验签的 appid；失败返回的 error 可用 errors.Is 与本包哨兵错误比对。
//
// 校验顺序：缺头 → 未知 appid → 时间戳越窗 → 签名不匹配。nonce 去重不在此处，
// 由中间件在 Verify 通过后配合 NonceStore 完成（与方案文档的中间件顺序一致）。
func (m *Manager) Verify(now time.Time, p Params) (string, error) {
	if p.AppID == "" || p.Timestamp == "" || p.Nonce == "" || p.Signature == "" {
		return "", ErrMissingSignature
	}

	secret, ok := m.keys[p.AppID]
	if !ok {
		return "", ErrUnknownAppID
	}

	ts, err := strconv.ParseInt(strings.TrimSpace(p.Timestamp), 10, 64)
	if err != nil {
		return "", fmt.Errorf("%w: invalid timestamp %q", ErrMissingSignature, p.Timestamp)
	}
	diff := now.Unix() - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(m.skew.Seconds()) {
		return "", ErrTimestampExpired
	}

	expected := Sign(secret, p)
	// hmac.Equal 为常量时间比较，避免按字节短路被时序侧信道探测出正确签名前缀。
	if !hmac.Equal([]byte(expected), []byte(p.Signature)) {
		return "", ErrInvalidSignature
	}
	return p.AppID, nil
}

// Sign 用 secret 对 p 计算大写 HEX(HMAC-SHA256(CanonicalRequest))。
// 既供 Verify 内部重算，也可供调用方 / 单测生成签名。
func Sign(secret string, p Params) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(CanonicalRequest(p)))
	return strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
}

// CanonicalRequest 构造规范请求串。字段顺序与分隔符必须与调用方完全一致。
func CanonicalRequest(p Params) string {
	return strings.Join([]string{
		Scheme,
		p.Method,
		p.Path,
		CanonicalQuery(p.RawQuery),
		hexSHA256(p.Body),
		strings.TrimSpace(p.Timestamp),
		p.Nonce,
	}, "\n")
}

// CanonicalQuery 把 query 串规范化：key 按字典序升序，同名 key 的多值再按值字典序升序，
// 用 "&" 拼回，key/value 均 url.QueryEscape。空 query 返回空串。
//
// 多值全部参与签名的原因：若只签首值，攻击者可在 "?a=1&a=2" 中追加 / 重排额外值而保持
// 签名不变——body 之外的"隐形通道"会被忽略。逐值签名彻底消除这一面。
func CanonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(values))
	for _, k := range keys {
		vs := append([]string(nil), values[k]...)
		sort.Strings(vs)
		ek := url.QueryEscape(k)
		for _, v := range vs {
			parts = append(parts, ek+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// ParseSignatureHeader 从 X-Auth-Sign 的值 "TC-HMAC-SHA256 Credential=<appid>,Signature=<SIG>"
// 解出 credential 与 signature。前缀不匹配时返回空串，由调用方据此回 401。
func ParseSignatureHeader(header string) (credential, signature string) {
	prefix := Scheme + " "
	if !strings.HasPrefix(header, prefix) {
		return "", ""
	}
	for kv := range strings.SplitSeq(strings.TrimPrefix(header, prefix), ",") {
		kv = strings.TrimSpace(kv)
		switch {
		case strings.HasPrefix(kv, "Credential="):
			credential = strings.TrimPrefix(kv, "Credential=")
		case strings.HasPrefix(kv, "Signature="):
			signature = strings.TrimPrefix(kv, "Signature=")
		}
	}
	return credential, signature
}

// hexSHA256 返回 body 的 SHA256 小写 hex（空 body 为 e3b0c442...b855，与调用方对齐）。
func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
