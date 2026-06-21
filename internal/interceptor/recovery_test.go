package interceptor

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRecoveryInterceptorCatchesPanic(t *testing.T) {
	interceptor := UnaryRecoveryInterceptor()

	_, err := interceptor(
		context.Background(),
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"},
		func(_ context.Context, _ any) (any, error) {
			panic("super-secret-dsn=postgres://user:pwd@host/db")
		},
	)

	if status.Code(err) != codes.Internal {
		t.Fatalf("panic 应转换为 Internal 错误, got code = %v (err=%v)", status.Code(err), err)
	}
	// 关键安全约束：panic 内容不得回写给客户端，避免泄露内部状态。
	if msg := status.Convert(err).Message(); strings.Contains(msg, "super-secret-dsn") {
		t.Fatalf("Recovery 返回的错误信息泄露了 panic 内容: %q", msg)
	}
}

func TestRecoveryInterceptorPassesThrough(t *testing.T) {
	interceptor := UnaryRecoveryInterceptor()

	want := "ok"
	resp, err := interceptor(
		context.Background(),
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"},
		func(_ context.Context, _ any) (any, error) {
			return want, nil
		},
	)
	if err != nil {
		t.Fatalf("正常调用不应返回错误: %v", err)
	}
	if resp != want {
		t.Fatalf("resp = %v, want %v", resp, want)
	}
}
