package otel

import (
	"context"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// meterProvider 进程级 MeterProvider 单例，供优雅关闭时刷新。
var meterProvider *sdkmetric.MeterProvider

// InitMetrics 装配全局 MeterProvider（Prometheus 拉取式 exporter）并启动 Go runtime
// 指标采集，返回供 /metrics 端点使用的 http.Handler。
//
// 关键点：必须在创建 otelgrpc / otelhttp instrumentation 之前调用，二者默认使用全局
// MeterProvider，调用本函数后即可自动产出 gRPC / HTTP 的 RED 指标（请求量 / 错误率 /
// 耗时分布），业务无需手动埋点。runtime 指标（GC、goroutine、堆内存等）由 OTel 官方
// contrib/instrumentation/runtime 采集。
func InitMetrics(serviceName string) (http.Handler, error) {
	// 独立 Registry，避免污染 prometheus 默认全局 Registry。
	reg := prometheus.NewRegistry()

	// Prometheus exporter 本身是一个 metric.Reader，并把采集器注册到上面的 Registry。
	exporter, err := promexporter.New(promexporter.WithRegisterer(reg))
	if err != nil {
		return nil, fmt.Errorf("otel: 创建 Prometheus exporter 失败: %w", err)
	}

	res := resource.NewSchemaless(attribute.String("service.name", serviceName))
	meterProvider = sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(meterProvider)

	// Go runtime 指标采集（基于全局 MeterProvider）。
	if err := otelruntime.Start(otelruntime.WithMeterProvider(meterProvider)); err != nil {
		return nil, fmt.Errorf("otel: 启动 runtime 指标采集失败: %w", err)
	}

	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}), nil
}

// ShutdownMetrics 刷新并关闭 MeterProvider，通常在程序优雅退出时调用。
func ShutdownMetrics(ctx context.Context) error {
	if meterProvider == nil {
		return nil
	}
	return meterProvider.Shutdown(ctx)
}
