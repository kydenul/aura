package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func doGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthzAlwaysOK(t *testing.T) {
	h := NewHandler(Options{})
	if rec := doGet(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", rec.Code)
	}
}

func TestReadyzWithoutCheck(t *testing.T) {
	h := NewHandler(Options{})
	if rec := doGet(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("/readyz(无回调) = %d, want 200", rec.Code)
	}
}

func TestReadyzCheckFails(t *testing.T) {
	h := NewHandler(Options{
		ReadinessCheck: func() error { return errors.New("db down") },
	})
	rec := doGet(t, h, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz(回调失败) = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); body == "" || body == "ok" {
		t.Errorf("/readyz 失败应返回原因, got %q", body)
	}
}

func TestMetricsMounting(t *testing.T) {
	called := false
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := NewHandler(Options{MetricsPath: "/metrics", MetricsHandler: metrics})

	if rec := doGet(t, h, "/metrics"); rec.Code != http.StatusOK || !called {
		t.Fatalf("/metrics 未正确挂载: code=%d called=%v", rec.Code, called)
	}
}

func TestMetricsDisabledWhenNil(t *testing.T) {
	h := NewHandler(Options{})
	if rec := doGet(t, h, "/metrics"); rec.Code != http.StatusNotFound {
		t.Fatalf("MetricsHandler 为 nil 时 /metrics 应 404, got %d", rec.Code)
	}
}

func TestPprofToggle(t *testing.T) {
	// 关闭时不应注册 pprof。
	off := NewHandler(Options{})
	if rec := doGet(t, off, "/debug/pprof/"); rec.Code != http.StatusNotFound {
		t.Fatalf("pprof 关闭时应 404, got %d", rec.Code)
	}

	// 开启时 index 可访问。
	on := NewHandler(Options{EnablePprof: true})
	if rec := doGet(t, on, "/debug/pprof/"); rec.Code != http.StatusOK {
		t.Fatalf("pprof 开启时 /debug/pprof/ 应 200, got %d", rec.Code)
	}
}

func TestNewServerWiring(t *testing.T) {
	srv := NewServer(Options{Addr: ":9099"})
	if srv.Addr != ":9099" {
		t.Errorf("Addr = %q, want :9099", srv.Addr)
	}
	if srv.Handler == nil {
		t.Error("Handler 不应为 nil")
	}
}
