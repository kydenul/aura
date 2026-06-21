package interceptor

import (
	"context"
	"testing"

	"aura/pkg/ratelimit"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func passHandler(_ context.Context, _ any) (any, error) { return "ok", nil }

// TestRateLimit_AllowWithinLimit 桶容量内放行并透传 handler 结果。
func TestRateLimit_AllowWithinLimit(t *testing.T) {
	itc := rateLimitUnary(ratelimit.New(true, 100, 5))
	info := &grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"}

	resp, err := itc(context.Background(), nil, info, passHandler)
	if err != nil {
		t.Fatalf("容量内不应被限流: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("resp=%v, want ok", resp)
	}
}

// TestRateLimit_RejectOverLimit 超出 burst 后返回 ResourceExhausted（网关会映射成 429）。
func TestRateLimit_RejectOverLimit(t *testing.T) {
	// rps 极低、burst=1：第 1 次放行，第 2 次必拒。
	itc := rateLimitUnary(ratelimit.New(true, 0.001, 1))
	info := &grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"}

	if _, err := itc(context.Background(), nil, info, passHandler); err != nil {
		t.Fatalf("第 1 次应放行: %v", err)
	}

	_, err := itc(context.Background(), nil, info, passHandler)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("第 2 次应被限流为 ResourceExhausted, got=%v (err=%v)", status.Code(err), err)
	}
}

// TestRateLimit_NilLimiterAllows limiter 为 nil 时恒放行（不限流）。
func TestRateLimit_NilLimiterAllows(t *testing.T) {
	itc := rateLimitUnary(nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"}

	for i := range 100 {
		if _, err := itc(context.Background(), nil, info, passHandler); err != nil {
			t.Fatalf("nil limiter 第 %d 次应放行: %v", i+1, err)
		}
	}
}

// TestRateLimit_DisabledAllows enabled=false 时恒放行。
func TestRateLimit_DisabledAllows(t *testing.T) {
	itc := rateLimitUnary(ratelimit.New(false, 0.001, 1))
	info := &grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"}

	for i := range 100 {
		if _, err := itc(context.Background(), nil, info, passHandler); err != nil {
			t.Fatalf("disabled 第 %d 次应放行: %v", i+1, err)
		}
	}
}

// TestRateLimit_HealthCheckBypassed 健康检查方法走白名单，即使桶已耗尽也不被限流，
// 避免探针被限流导致实例被误判摘流。
func TestRateLimit_HealthCheckBypassed(t *testing.T) {
	itc := UnaryRateLimitInterceptor(ratelimit.New(true, 0.001, 1))
	health := &grpc.UnaryServerInfo{FullMethod: healthCheckMethod}

	// 即便业务方法早已被限流，健康检查仍应连续放行。
	for i := range 50 {
		if _, err := itc(context.Background(), nil, health, passHandler); err != nil {
			t.Fatalf("健康检查第 %d 次应始终放行（白名单）: %v", i+1, err)
		}
	}
}
