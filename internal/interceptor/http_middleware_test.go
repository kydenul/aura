package interceptor

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORSMiddlewarePreflight(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := CORSMiddleware(next)

	req := httptest.NewRequest(http.MethodOptions, "/api/users", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestCORSMiddlewarePassThrough(t *testing.T) {
	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := CORSMiddleware(next)

	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("普通请求应透传到下游 handler")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("普通请求也应带 CORS 头, got %q", got)
	}
}

// 显式收敛白名单时，非白名单 Origin 不应回带 Allow-Origin。
func TestCORSMiddlewareWithExplicitWhitelist(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := CORSMiddlewareWith(CORSOptions{
		AllowedOrigins: []string{"https://allowed.example.com"},
	}, next)

	t.Run("白名单 Origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
		req.Header.Set("Origin", "https://allowed.example.com")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example.com" {
			t.Errorf("白名单 Origin 应被回带, got %q", got)
		}
	})

	t.Run("非白名单 Origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("非白名单 Origin 不应带 Allow-Origin, got %q", got)
		}
	})
}

func TestHTTPLoggingMiddleware(t *testing.T) {
	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	})
	handler := HTTPLoggingMiddleware(next)

	req := httptest.NewRequest(http.MethodPost, "/api/users", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("日志中间件应透传到下游 handler")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}
