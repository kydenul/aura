package interceptor

import (
	"context"

	authjwt "aura/pkg/jwt"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors"
	grpcauth "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/auth"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/selector"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 不需要鉴权的方法白名单（按 FullMethod 匹配，例如健康检查）
var authWhitelist = map[string]bool{
	healthCheckMethod: true,
}

// UnaryAuthInterceptor 返回一个 gRPC 一元拦截器，组合社区成熟库实现 JWT 鉴权：
//   - auth.AuthFromMD 标准化地从 "authorization" metadata 提取 Bearer token
//   - pkg/jwt.Manager 校验签名/过期/issuer 并解析业务 Claims
//   - selector.MatchFunc 实现"白名单方法跳过鉴权"的能力
//
// HTTP 请求经 grpc-gateway 转发时 Authorization header 会通过 metadata 透传到这里，
// 因此同一份鉴权逻辑可同时覆盖 gRPC 与 HTTP 两个入口。
func UnaryAuthInterceptor(jwtManager *authjwt.Manager) grpc.UnaryServerInterceptor {
	return selector.UnaryServerInterceptor(
		grpcauth.UnaryServerInterceptor(authFunc(jwtManager)),
		selector.MatchFunc(func(_ context.Context, c interceptors.CallMeta) bool {
			return !authWhitelist[c.FullMethod()]
		}),
	)
}

// authFunc 返回 go-grpc-middleware auth 包要求的鉴权函数：
// 校验通过后把解析出的 Claims 注入 context，下游 handler 可用 jwt.FromContext 取用。
func authFunc(jwtManager *authjwt.Manager) grpcauth.AuthFunc {
	return func(ctx context.Context) (context.Context, error) {
		token, err := grpcauth.AuthFromMD(ctx, "bearer")
		if err != nil {
			return nil, err
		}

		claims, err := jwtManager.Parse(token)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
		}

		return authjwt.NewContext(ctx, claims), nil
	}
}
