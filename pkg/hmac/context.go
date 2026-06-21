package hmac

import "context"

// ctxKey 是私有类型，避免与其他包的 context key 冲突。
type ctxKey struct{}

// NewContext 把通过验签的调用方 appid 注入到 context，供下游 handler 取用。
func NewContext(ctx context.Context, appID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, appID)
}

// FromContext 从 context 取出调用方 appid，第二个返回值表示是否存在。
func FromContext(ctx context.Context) (string, bool) {
	appID, ok := ctx.Value(ctxKey{}).(string)
	return appID, ok
}
