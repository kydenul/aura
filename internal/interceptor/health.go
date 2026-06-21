package interceptor

// gRPC 标准健康检查服务（grpc.health.v1.Health）的 FullMethod 名称。
// 鉴权与限流白名单均按 FullMethod 匹配，统一引用这些常量避免散落的魔法字符串。
const (
	healthCheckMethod = "/grpc.health.v1.Health/Check"
	healthWatchMethod = "/grpc.health.v1.Health/Watch"
)
