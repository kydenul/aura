package jwt

import "context"

// ctxKey 是私有类型，避免与其他包的 context key 冲突。
type ctxKey struct{}

// NewContext 把校验通过的 Claims 注入到 context，供下游 handler 取用。
func NewContext(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, claims)
}

// FromContext 从 context 取出 Claims，第二个返回值表示是否存在。
func FromContext(ctx context.Context) (*Claims, bool) {
	claims, ok := ctx.Value(ctxKey{}).(*Claims)
	return claims, ok
}
