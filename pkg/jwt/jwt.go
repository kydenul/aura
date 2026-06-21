// Package jwt 提供一个可复用的 JWT 组件，封装基于 github.com/golang-jwt/jwt/v5
// 的 token 签发与校验能力。它与具体框架解耦，gRPC 拦截器、HTTP middleware
// 或任何业务代码都可以直接复用同一个 Manager 实例。
package jwt

import (
	"errors"
	"fmt"
	"slices"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// 组件对外暴露的错误，调用方可用 errors.Is 精确判断失败原因。
var (
	// ErrEmptySecret 创建 Manager 时未提供签名密钥。
	ErrEmptySecret = errors.New("jwt: secret must not be empty")
	// ErrInvalidToken token 非法（签名错误、格式损坏、issuer 不匹配等）。
	ErrInvalidToken = errors.New("jwt: invalid token")
	// ErrTokenExpired token 已过期。
	ErrTokenExpired = errors.New("jwt: token expired")
)

// Claims 是自定义的业务声明，内嵌标准 RegisteredClaims（exp/iat/iss/sub 等）。
// 业务字段使用短 key 以减小 token 体积。
type Claims struct {
	UserID string   `json:"uid"`
	Name   string   `json:"name,omitempty"`
	Email  string   `json:"email,omitempty"`
	Roles  []string `json:"roles,omitempty"`

	jwtv5.RegisteredClaims
}

// HasRole 判断 claims 是否包含指定角色，便于下游做粗粒度鉴权。
func (c *Claims) HasRole(role string) bool {
	return slices.Contains(c.Roles, role)
}

// Config 是 Manager 的配置项。
type Config struct {
	// Secret HMAC 签名密钥，必填。生产环境务必从配置/密钥管理服务注入，不要硬编码。
	Secret string
	// Issuer 签发者标识，写入 iss 并在校验时强制比对（为空则不校验 iss）。
	Issuer string
	// TTL token 有效期，<=0 时使用默认值 DefaultTTL。
	TTL time.Duration
}

// DefaultTTL 是未显式配置 TTL 时使用的默认有效期。
const DefaultTTL = 2 * time.Hour

// Manager 负责 token 的签发与校验，创建后并发安全，可作为单例长期持有。
type Manager struct {
	secret []byte
	issuer string
	ttl    time.Duration
	method jwtv5.SigningMethod
	parser *jwtv5.Parser
}

// NewManager 根据配置创建一个 Manager。
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Secret == "" {
		return nil, ErrEmptySecret
	}

	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	m := &Manager{
		secret: []byte(cfg.Secret),
		issuer: cfg.Issuer,
		ttl:    ttl,
		method: jwtv5.SigningMethodHS256,
	}

	// 解析时强制限定签名算法，杜绝 alg=none 以及 RS/HS 算法混淆攻击；
	// 并按需开启过期、签发者校验。
	parserOpts := []jwtv5.ParserOption{
		jwtv5.WithValidMethods([]string{m.method.Alg()}),
		jwtv5.WithExpirationRequired(),
	}
	if cfg.Issuer != "" {
		parserOpts = append(parserOpts, jwtv5.WithIssuer(cfg.Issuer))
	}
	m.parser = jwtv5.NewParser(parserOpts...)

	return m, nil
}

// Generate 根据业务信息签发一个签名后的 token 字符串。
func (m *Manager) Generate(userID, name, email string, roles ...string) (string, error) {
	now := time.Now()
	claims := &Claims{
		UserID: userID,
		Name:   name,
		Email:  email,
		Roles:  roles,
		RegisteredClaims: jwtv5.RegisteredClaims{
			Subject:   userID,
			Issuer:    m.issuer,
			IssuedAt:  jwtv5.NewNumericDate(now),
			NotBefore: jwtv5.NewNumericDate(now),
			ExpiresAt: jwtv5.NewNumericDate(now.Add(m.ttl)),
		},
	}

	token := jwtv5.NewWithClaims(m.method, claims)
	signed, err := token.SignedString(m.secret)
	if err != nil {
		return "", fmt.Errorf("jwt: sign token: %w", err)
	}
	return signed, nil
}

// Parse 校验并解析 token，成功时返回业务 Claims。
// 失败时返回的 error 可用 errors.Is 与 ErrTokenExpired / ErrInvalidToken 比对。
func (m *Manager) Parse(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := m.parser.ParseWithClaims(tokenString, claims, func(_ *jwtv5.Token) (any, error) {
		return m.secret, nil
	})
	if err != nil {
		if errors.Is(err, jwtv5.ErrTokenExpired) {
			return nil, fmt.Errorf("%w: %v", ErrTokenExpired, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
