package log

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestWithFieldsMergeAndOverride(t *testing.T) {
	ctx := context.Background()
	ctx = WithFields(ctx, String("a", "1"))
	ctx = WithFields(ctx, String("b", "2"))
	ctx = WithFields(ctx, String("a", "override")) // 同名覆盖

	got := map[string]string{}
	for _, f := range Fields(ctx) {
		got[f.Key] = f.String
	}
	if got["a"] != "override" {
		t.Errorf("a = %q, want override", got["a"])
	}
	if got["b"] != "2" {
		t.Errorf("b = %q, want 2", got["b"])
	}
	if len(got) != 2 {
		t.Errorf("字段数量 = %d, want 2（同名应覆盖而非新增）", len(got))
	}
}

func TestWithTraceAndRequestID(t *testing.T) {
	ctx := WithRequestID(WithTraceID(context.Background(), "trace-abc"), "req-123")
	if TraceID(ctx) != "trace-abc" {
		t.Errorf("TraceID = %q, want trace-abc", TraceID(ctx))
	}
	if RequestID(ctx) != "req-123" {
		t.Errorf("RequestID = %q, want req-123", RequestID(ctx))
	}

	// 空值不应污染 context。
	if got := WithTraceID(context.Background(), ""); TraceID(got) != "" {
		t.Error("空 traceID 不应写入 context")
	}
}

func TestInfoContextOutputsTraceFields(t *testing.T) {
	var buf bytes.Buffer
	restore := SetOutputForTesting(&buf)
	defer restore()

	ctx := WithRequestID(WithTraceID(context.Background(), "trace-xyz"), "req-789")
	InfoContext(ctx, "hello", String("k", "v"))
	InfofContext(ctx, "world %d", 42)

	out := buf.String()
	for _, want := range []string{"trace-xyz", "req-789", "hello", "world 42", "trace_id", "request_id"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志输出缺少 %q\n实际:\n%s", want, out)
		}
	}
}

func TestNilContextSafe(t *testing.T) {
	if Fields(nil) != nil { //nolint:staticcheck // 显式验证 nil context 的健壮性
		t.Error("nil context 的 Fields 应返回 nil")
	}
	// 不应 panic
	InfoContext(context.Background(), "ok")
}
