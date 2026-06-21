package interceptor

import (
	"bytes"
	"strings"
	"testing"

	"aura/pkg/log"

	grpclogging "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
)

// TestZapLogAdapterContextFields 验证适配器会把 context 链路字段与本次调用字段一起输出。
func TestZapLogAdapterContextFields(t *testing.T) {
	var buf bytes.Buffer
	restore := log.SetOutputForTesting(&buf)
	defer restore()

	ctx := log.WithTraceID(t.Context(), "trace-abc")
	logger := zapLogAdapter()
	logger.Log(ctx, grpclogging.LevelInfo, "grpc finished", "grpc.method", "GetUser", "grpc.code", "OK")

	out := buf.String()
	for _, want := range []string{"trace-abc", "trace_id", "grpc finished", "grpc.method", "GetUser"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志缺少 %q\n实际:\n%s", want, out)
		}
	}
}

// TestZapLogAdapterLevels 验证不同级别均能正常输出（不 panic、内容可见）。
func TestZapLogAdapterLevels(t *testing.T) {
	var buf bytes.Buffer
	restore := log.SetOutputForTesting(&buf)
	defer restore()
	if err := log.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel error: %v", err)
	}
	defer func() { _ = log.SetLevel("info") }()

	logger := zapLogAdapter()
	cases := []struct {
		lvl grpclogging.Level
		msg string
	}{
		{grpclogging.LevelDebug, "dbg-msg"},
		{grpclogging.LevelInfo, "info-msg"},
		{grpclogging.LevelWarn, "warn-msg"},
		{grpclogging.LevelError, "error-msg"},
	}
	for _, c := range cases {
		logger.Log(t.Context(), c.lvl, c.msg)
	}

	out := buf.String()
	for _, c := range cases {
		if !strings.Contains(out, c.msg) {
			t.Errorf("级别 %v 的日志缺少 %q\n实际:\n%s", c.lvl, c.msg, out)
		}
	}
}

// TestZapLogAdapterOddKV 验证奇数个 kv 时不越界（最后一个孤立 key 被安全忽略）。
func TestZapLogAdapterOddKV(t *testing.T) {
	var buf bytes.Buffer
	restore := log.SetOutputForTesting(&buf)
	defer restore()

	logger := zapLogAdapter()
	// 三个参数：一对 + 一个孤立 key，不应 panic。
	logger.Log(t.Context(), grpclogging.LevelInfo, "msg", "k1", "v1", "orphan")

	out := buf.String()
	if !strings.Contains(out, "v1") {
		t.Errorf("成对字段应输出, 实际:\n%s", out)
	}
}

// TestUnaryLoggingInterceptorConstruct 验证构造函数返回非 nil 拦截器。
func TestUnaryLoggingInterceptorConstruct(t *testing.T) {
	if UnaryLoggingInterceptor() == nil {
		t.Fatal("UnaryLoggingInterceptor 返回 nil")
	}
}
