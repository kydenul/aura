package interceptor

import (
	"context"
	"testing"
	"time"

	authjwt "aura/pkg/jwt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// newTestJWTManager 构造一个用于测试的 jwt.Manager。
func newTestJWTManager(t *testing.T) *authjwt.Manager {
	t.Helper()
	m, err := authjwt.NewManager(authjwt.Config{Secret: "test-secret", Issuer: "aura-test", TTL: time.Hour})
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}
	return m
}

// ctxWithBearer 构造携带 Authorization metadata 的 incoming context。
func ctxWithBearer(token string) context.Context {
	md := metadata.Pairs("authorization", "bearer "+token)
	return metadata.NewIncomingContext(context.Background(), md)
}

// okHandler 是一个记录是否被调用的 handler，便于断言鉴权是否放行。
func okHandler(called *bool) grpc.UnaryHandler {
	return func(ctx context.Context, _ any) (any, error) {
		*called = true
		return ctx, nil // 返回 ctx，便于断言 claims 是否注入
	}
}

func TestAuthInterceptorValidToken(t *testing.T) {
	m := newTestJWTManager(t)
	token, err := m.Generate("u-1", "alice", "alice@example.com", "admin")
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	interceptor := UnaryAuthInterceptor(m)
	var called bool
	resp, err := interceptor(
		ctxWithBearer(token),
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"},
		okHandler(&called),
	)
	if err != nil {
		t.Fatalf("鉴权应通过, got err: %v", err)
	}
	if !called {
		t.Fatal("handler 未被调用")
	}

	// 校验通过后 claims 应注入到下游 context。
	ctx, ok := resp.(context.Context)
	if !ok {
		t.Fatalf("handler 返回类型异常: %T", resp)
	}
	claims, ok := authjwt.FromContext(ctx)
	if !ok {
		t.Fatal("claims 未注入 context")
	}
	if claims.UserID != "u-1" || !claims.HasRole("admin") {
		t.Errorf("注入的 claims 不正确: %+v", claims)
	}
}

func TestAuthInterceptorInvalidToken(t *testing.T) {
	m := newTestJWTManager(t)
	interceptor := UnaryAuthInterceptor(m)

	var called bool
	_, err := interceptor(
		ctxWithBearer("this-is-not-a-valid-token"),
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"},
		okHandler(&called),
	)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err code = %v, want Unauthenticated", status.Code(err))
	}
	if called {
		t.Fatal("非法 token 不应放行 handler")
	}
}

func TestAuthInterceptorMissingToken(t *testing.T) {
	m := newTestJWTManager(t)
	interceptor := UnaryAuthInterceptor(m)

	var called bool
	// 无 metadata，缺少 authorization。
	_, err := interceptor(
		context.Background(),
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"},
		okHandler(&called),
	)
	if err == nil {
		t.Fatal("缺少 token 应返回错误")
	}
	if called {
		t.Fatal("缺少 token 不应放行 handler")
	}
}

func TestAuthInterceptorWhitelistSkips(t *testing.T) {
	m := newTestJWTManager(t)
	interceptor := UnaryAuthInterceptor(m)

	var called bool
	// 白名单方法即使没有 token 也应直接放行。
	_, err := interceptor(
		context.Background(),
		nil,
		&grpc.UnaryServerInfo{FullMethod: healthCheckMethod},
		okHandler(&called),
	)
	if err != nil {
		t.Fatalf("白名单方法不应鉴权, got err: %v", err)
	}
	if !called {
		t.Fatal("白名单方法应直接放行 handler")
	}
}
