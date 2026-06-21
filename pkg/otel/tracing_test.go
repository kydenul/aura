package otel

import (
	"context"
	"slices"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestInitTracing(t *testing.T) {
	err := InitTracing(context.Background(), TracingOptions{
		ServiceName: "aura-test",
		Exporter:    ExporterNone,
		SampleRatio: 1.0,
	})
	if err != nil {
		t.Fatalf("InitTracing error: %v", err)
	}
	t.Cleanup(func() { _ = ShutdownTracing(context.Background()) })

	// 初始化后，全局 propagator 应支持 W3C traceparent 注入。
	prop := otel.GetTextMapPropagator()
	if !slices.Contains(prop.Fields(), "traceparent") {
		t.Errorf("propagator 应包含 traceparent 字段, got %v", prop.Fields())
	}

	// SampleRatio>=1：每条 span 都应生成有效 SpanContext（含 TraceID / SpanID）。
	tracer := otel.Tracer("test")
	_, span := tracer.Start(context.Background(), "op")
	sc := span.SpanContext()
	span.End()

	if !sc.IsValid() {
		t.Fatal("全采样下 SpanContext 应有效")
	}
	if !sc.HasTraceID() || !sc.HasSpanID() {
		t.Errorf("SpanContext 应携带 TraceID/SpanID: %+v", sc)
	}
}

// TestSamplerFor 校验采样比例到 Sampler 的映射边界。
func TestSamplerFor(t *testing.T) {
	cases := []struct {
		ratio float64
		want  string
	}{
		{1.0, sdktrace.ParentBased(sdktrace.AlwaysSample()).Description()},
		{0, sdktrace.ParentBased(sdktrace.NeverSample()).Description()},
		{0.25, sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.25)).Description()},
	}
	for _, c := range cases {
		if got := samplerFor(c.ratio).Description(); got != c.want {
			t.Errorf("samplerFor(%v) = %q, want %q", c.ratio, got, c.want)
		}
	}
}

func TestShutdownTracing(t *testing.T) {
	if err := InitTracing(context.Background(), TracingOptions{ServiceName: "aura-test", SampleRatio: 1}); err != nil {
		t.Fatalf("InitTracing error: %v", err)
	}
	if err := ShutdownTracing(context.Background()); err != nil {
		t.Fatalf("ShutdownTracing error: %v", err)
	}
}

// TestShutdownTracingNilSafe 验证 provider 为 nil（未初始化）时关闭安全。
func TestShutdownTracingNilSafe(t *testing.T) {
	prev := provider
	provider = nil
	t.Cleanup(func() { provider = prev })

	if err := ShutdownTracing(context.Background()); err != nil {
		t.Fatalf("未初始化时 ShutdownTracing 应返回 nil, got %v", err)
	}
}
