package interceptor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	authhmac "aura/pkg/hmac"
	"aura/pkg/log"
	"aura/pkg/redis"
)

// maxHMACBodyBytes 验签时读入内存的请求体上限（10MB，与方案文档一致）。
// 验签需要对完整 body 做 SHA256，必须先整体读出；用 LimitReader 兜底防止超大 body 打爆内存。
const maxHMACBodyBytes = 10 << 20

// HMACAuthMiddlewareWith 校验外层 TC-HMAC-SHA256 调用方签名并把 appid 注入 ctx。
//
// 为什么是 HTTP middleware 而非 gRPC 拦截器（与 JWT 不同）：
// HMAC 把请求 body 纳入签名，而 grpc-gateway 会把 HTTP/JSON body 重新编码为 protobuf
// 再经 loopback gRPC 转发，body 字节已变，gRPC 侧无法复现签名串；因此 body-inclusive
// 的 HMAC 只能在 HTTP 入口层校验。签名走独立头 X-Auth-Sign，不占用 Authorization
// （Authorization: Bearer 留给 gRPC 层 JWT，两层鉴权互不干扰）。
//
// protectedPrefixes 控制生效范围：为空时保护全部路由；非空时仅对命中任一前缀的请求验签，
// 其余请求直接放行交给后续（如 JWT）处理——避免对所有用户接口强制双重鉴权。
//
// 流程：
//  1. 命中保护前缀才继续；完整读出 body（验签需对 body 做 SHA256），读完回填 r.Body 供下游复用；
//  2. 解析 X-Auth-Sign 头取签名，连同 X-Auth-AppId/Timestamp/Nonce 调 mgr.Verify；
//  3. Verify 通过后用 nonces 做 SETNX 防重放（nonces 为 nil 时跳过，仅靠时间窗）；
//  4. 成功 → ctx 注入 appid（hmac.NewContext），next.ServeHTTP；
//  5. 失败 → 回 HTTP 401 + grpc-gateway 形状错误体，不区分具体原因（防探测），日志区分级别。
func HMACAuthMiddlewareWith(
	mgr *authhmac.Manager,
	nonces authhmac.NonceStore,
	protectedPrefixes []string,
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !pathProtected(r.URL.Path, protectedPrefixes) {
			next.ServeHTTP(w, r)
			return
		}

		body, ok := readAndRestoreBody(w, r)
		if !ok {
			return
		}

		_, signature := authhmac.ParseSignatureHeader(r.Header.Get(authhmac.HeaderSignature))
		appID := r.Header.Get(authhmac.HeaderAppID)
		nonce := r.Header.Get(authhmac.HeaderNonce)

		if _, err := mgr.Verify(time.Now(), authhmac.Params{
			Method:    r.Method,
			Path:      r.URL.Path,
			RawQuery:  r.URL.RawQuery,
			AppID:     appID,
			Timestamp: r.Header.Get(authhmac.HeaderTimestamp),
			Nonce:     nonce,
			Signature: signature,
			Body:      body,
		}); err != nil {
			logHMACReject(r, appID, err)
			writeHMACUnauthorized(w)
			return
		}

		// 防重放：Verify 通过后再做 nonce 去重（顺序与方案文档一致）。
		// TTL 取 2×Skew：Verify 的窗口判定是开区间（|diff|>skew 才拒），但 Redis EX 是
		// 闭区间的「整秒过期」，二者叠加会出现「ts 仍在窗口、nonce 已过期」的边界缝隙，
		// 攻击者可在此缝隙里重放同一签名。乘 2 留足过期精度与时钟漂移余量，确保任何
		// 「仍可通过时间窗的请求」都能被 nonce 命中。
		if !checkNonce(r, nonces, appID, nonce, 2*mgr.Skew()) {
			writeHMACUnauthorized(w)
			return
		}

		next.ServeHTTP(w, r.WithContext(authhmac.NewContext(r.Context(), appID)))
	})
}

// pathProtected 判断请求路径是否需要 HMAC 校验：prefixes 为空表示保护全部；
// 否则命中任一前缀才保护。
func pathProtected(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// checkNonce 通过 NonceStore 登记 (appid,nonce)，重放或存储异常返回 false（拒绝）。
// nonces 为 nil（如 Redis 未启用）时跳过去重，仅靠时间窗防重放，返回 true。
func checkNonce(r *http.Request, nonces authhmac.NonceStore, appID, nonce string, ttl time.Duration) bool {
	if nonces == nil {
		return true
	}
	first, err := nonces.FirstSeen(r.Context(), appID, nonce, ttl)
	if err != nil {
		log.ErrorfContext(r.Context(), "[HMAC] nonce store error: path=%s appid=%s err=%v", r.URL.Path, appID, err)
		return false
	}
	if !first {
		log.WarnfContext(r.Context(), "[HMAC] nonce replay: path=%s appid=%s nonce=%s", r.URL.Path, appID, nonce)
		return false
	}
	return true
}

// readAndRestoreBody 完整读出请求体并回填 r.Body，便于下游 mux/handler 再次读取。
// 读失败或超过 maxHMACBodyBytes 时直接回 401 并返回 ok=false。
func readAndRestoreBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxHMACBodyBytes+1))
	if err != nil {
		log.ErrorfContext(r.Context(), "[HMAC] read body failed: path=%s err=%v", r.URL.Path, err)
		writeHMACUnauthorized(w)
		return nil, false
	}
	if len(b) > maxHMACBodyBytes {
		log.WarnfContext(r.Context(), "[HMAC] body too large: path=%s size>%d", r.URL.Path, maxHMACBodyBytes)
		writeHMACUnauthorized(w)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, true
}

// logHMACReject 按失败类型分级打日志（对外仍统一回 401，仅服务端日志区分原因便于排障）。
func logHMACReject(r *http.Request, appID string, err error) {
	ctx := r.Context()
	switch {
	case errors.Is(err, authhmac.ErrMissingSignature):
		log.DebugfContext(ctx, "[HMAC] missing/invalid signature headers: path=%s", r.URL.Path)
	case errors.Is(err, authhmac.ErrUnknownAppID):
		log.WarnfContext(ctx, "[HMAC] unknown app id: path=%s appid=%s", r.URL.Path, appID)
	case errors.Is(err, authhmac.ErrTimestampExpired):
		log.WarnfContext(ctx, "[HMAC] timestamp out of window: path=%s appid=%s ts=%s",
			r.URL.Path, appID, r.Header.Get(authhmac.HeaderTimestamp))
	default:
		log.WarnfContext(ctx, "[HMAC] signature mismatch: path=%s appid=%s", r.URL.Path, appID)
	}
}

// writeHMACUnauthorized 回 HTTP 401，错误体形状与 grpc-gateway 默认错误一致
// （code 为 gRPC 状态码，16 = Unauthenticated），便于调用方按统一格式处理错误。
func writeHMACUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    16,
		"message": "unauthenticated: invalid request signature",
	})
}

// RedisNonceStore 基于 Redis SETNX 的 hmac.NonceStore 实现（对齐方案文档：
// SETNX hmac_nonce_<appid>_<nonce> 1 EX <ttl>）。复用 pkg/redis 单例，不引入新依赖。
type RedisNonceStore struct {
	client *redis.Client
}

// NewRedisNonceStore 用给定 Redis 客户端构造去重存储；client 为 nil 时返回 nil 接口
// （调用方可直接把结果传给中间件，中间件将跳过去重）。返回接口类型以规避 typed-nil 陷阱。
func NewRedisNonceStore(client *redis.Client) authhmac.NonceStore {
	if client == nil {
		return nil
	}
	return &RedisNonceStore{client: client}
}

// FirstSeen 用 SETNX 原子登记 nonce：首次返回 true，已存在返回 false（重放）。
func (s *RedisNonceStore) FirstSeen(ctx context.Context, appID, nonce string, ttl time.Duration) (bool, error) {
	key := "hmac_nonce_" + appID + "_" + nonce
	return s.client.SetNX(ctx, key, 1, ttl)
}
