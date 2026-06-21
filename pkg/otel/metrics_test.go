package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInitMetrics(t *testing.T) {
	handler, err := InitMetrics("aura-test")
	if err != nil {
		t.Fatalf("InitMetrics error: %v", err)
	}
	t.Cleanup(func() { _ = ShutdownMetrics(context.Background()) })

	if handler == nil {
		t.Fatal("InitMetrics 应返回非 nil 的 /metrics handler")
	}

	// /metrics 端点应可被抓取，且返回 Prometheus 文本格式。
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics 状态码 = %d, want 200", rec.Code)
	}
	// runtime 指标已启动，输出中应包含 OTel 资源信息 target_info 或 runtime 指标。
	body := rec.Body.String()
	if !strings.Contains(body, "target_info") && !strings.Contains(body, "go_") {
		t.Errorf("/metrics 输出缺少预期指标, got:\n%s", body)
	}
}

// TestShutdownMetricsNilSafe 验证未初始化时关闭安全。
func TestShutdownMetricsNilSafe(t *testing.T) {
	prev := meterProvider
	meterProvider = nil
	t.Cleanup(func() { meterProvider = prev })

	if err := ShutdownMetrics(context.Background()); err != nil {
		t.Fatalf("未初始化时 ShutdownMetrics 应返回 nil, got %v", err)
	}
}
