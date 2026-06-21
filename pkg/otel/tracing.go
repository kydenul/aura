// Package otel 封装可观测性基础设施的初始化：基于 OpenTelemetry 的分布式链路追踪
// （tracing.go）与指标采集（metrics.go）。
//
// 设计：
//   - 链路解析 / TraceID 生成 / 采样 / 跨进程传播全部交给成熟的 OpenTelemetry 生态
//     （otelgrpc / otelhttp + W3C TraceContext 传播器），不再自研。
//   - span 上报方式可配置：默认 "none"（仅进程内生成并传播 trace_id，满足"日志带
//     trace_id"的诉求，零外部依赖即可运行）；需要上报时配置 "otlp" 指向 collector
//     （Jaeger / Tempo / 伽利略 / OTLP 后端），调用方无感。
package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// 链路上报方式（TracingOptions.Exporter 取值）。
const (
	ExporterNone = "none" // 仅进程内生成 / 传播 trace_id，不向后端上报
	ExporterOTLP = "otlp" // 通过 OTLP gRPC 上报到 collector
)

// provider 进程级 TracerProvider 单例，供优雅关闭时刷新。
var provider *sdktrace.TracerProvider

// TracingOptions 链路追踪初始化参数（由 cmd/server 从 config 映射而来，
// 本包不反向依赖 config，保持可复用）。
type TracingOptions struct {
	ServiceName string  // 资源属性 service.name
	Exporter    string  // none | otlp
	Endpoint    string  // OTLP collector 地址（exporter=otlp 时必填）
	Insecure    bool    // OTLP 是否明文连接
	SampleRatio float64 // 采样比例 [0,1]
}

// InitTracing 初始化全局 TracerProvider 与传播器，通常在程序启动时调用一次。
//
// 采样由 SampleRatio 控制：>=1 全采样，<=0 不采样，其余按 TraceID 比例采样。注意无论是否
// 采样，SpanContext 都携带有效 TraceID/SpanID，因此日志的 trace_id 始终可用；采样只决定
// 该 span 是否被导出到后端。
func InitTracing(ctx context.Context, opts TracingOptions) error {
	res := resource.NewSchemaless(attribute.String("service.name", opts.ServiceName))

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(samplerFor(opts.SampleRatio)),
	}

	if opts.Exporter == ExporterOTLP {
		exporter, err := newOTLPExporter(ctx, opts)
		if err != nil {
			return fmt.Errorf("otel: 创建 OTLP trace exporter 失败: %w", err)
		}
		// BatchSpanProcessor：批量异步上报，降低对请求路径的影响。
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exporter))
	}

	provider = sdktrace.NewTracerProvider(tpOpts...)

	otel.SetTracerProvider(provider)
	// W3C TraceContext（traceparent）负责跨进程传播；Baggage 便于透传业务维度标签。
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return nil
}

// samplerFor 把采样比例映射为 OTel Sampler；按 ParentBased 包裹，尊重上游采样决策。
func samplerFor(ratio float64) sdktrace.Sampler {
	switch {
	case ratio >= 1:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case ratio <= 0:
		return sdktrace.ParentBased(sdktrace.NeverSample())
	default:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}

// newOTLPExporter 创建上报到 OTLP collector 的 gRPC span exporter。
func newOTLPExporter(ctx context.Context, opts TracingOptions) (sdktrace.SpanExporter, error) {
	grpcOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(opts.Endpoint)}
	if opts.Insecure {
		grpcOpts = append(grpcOpts, otlptracegrpc.WithInsecure())
	}
	return otlptracegrpc.New(ctx, grpcOpts...)
}

// ShutdownTracing 刷新并关闭 TracerProvider，通常在程序优雅退出时调用。
func ShutdownTracing(ctx context.Context) error {
	if provider == nil {
		return nil
	}
	return provider.Shutdown(ctx)
}
