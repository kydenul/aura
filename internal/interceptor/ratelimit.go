package interceptor

import (
	"context"

	"aura/pkg/ratelimit"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/selector"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rateLimitWhitelist 不参与限流的方法（按 FullMethod 匹配）。
// 健康检查必须始终放行：探针被限流会让 K8s / 负载均衡误判实例不健康而摘流，
// 在洪峰时雪上加霜。
var rateLimitWhitelist = map[string]bool{
	healthCheckMethod: true,
	healthWatchMethod: true,
}

// UnaryRateLimitInterceptor 返回一个 gRPC 一元拦截器，用 pkg/ratelimit 的单机令牌桶
// 做整机过载保护，防止瞬时洪峰把进程 / 下游（DB、Redis、协程）打崩。
//
// 为什么只挂 gRPC 一侧：HTTP REST 经 grpc-gateway 同进程 loopback 转发到 gRPC，
// 也会穿过本拦截器链，因此这一道限流同时覆盖 gRPC 直连与 HTTP 两个入口
// （与 UnaryAuthInterceptor 同款契约），HTTP 中间件无需重复挂，否则会双重计数。
//
// 被限流时返回 codes.ResourceExhausted；grpc-gateway 会把它映射为 HTTP 429 Too Many Requests。
//
// 链路位置（见 cmd/server/main.go）：recovery → trace → logging → ratelimit → auth → handler。
//   - 放在 logging 之后：被限流的请求仍带 trace_id 并被访问日志记录，便于观测限流命中；
//   - 放在 auth 之前：洪峰时在昂贵的 JWT 校验之前就拒绝，最大化保护 CPU。
//
// limiter 为 nil、或其 enabled=false / rps<=0 时恒放行（ratelimit.Limiter.Allow 已内建该语义）。
func UnaryRateLimitInterceptor(limiter *ratelimit.Limiter) grpc.UnaryServerInterceptor {
	return selector.UnaryServerInterceptor(
		rateLimitUnary(limiter),
		selector.MatchFunc(func(_ context.Context, c interceptors.CallMeta) bool {
			return !rateLimitWhitelist[c.FullMethod()]
		}),
	)
}

// rateLimitUnary 是实际执行限流判定的拦截器：放行则继续调用 handler，否则直接返回
// ResourceExhausted。这是「全局」维度（整机一个桶）；若需按 user_id 维度限流，可改用
// ratelimit.KeyedLimiter，并从 jwt.FromContext(ctx) 取 subject 作为 key（须置于 auth 之后）。
func rateLimitUnary(limiter *ratelimit.Limiter) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		_ *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if !limiter.Allow() {
			return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
		}
		return handler(ctx, req)
	}
}
