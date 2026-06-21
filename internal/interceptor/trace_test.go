package interceptor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aura/pkg/log"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testTraceIDHex = "0123456789abcdef0123456789abcdef"
	testSpanIDHex  = "0123456789abcdef"
)

// validSpanContext 构造一个有效的 SpanContext，供链路相关测试复用。
func validSpanContext(t *testing.T) trace.SpanContext {
	t.Helper()
	traceID, err := trace.TraceIDFromHex(testTraceIDHex)
	if err != nil {
		t.Fatalf("TraceIDFromHex error: %v", err)
	}
	spanID, err := trace.SpanIDFromHex(testSpanIDHex)
	if err != nil {
		t.Fatalf("SpanIDFromHex error: %v", err)
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
}

func TestInjectTraceFieldsValid(t *testing.T) {
	sc := validSpanContext(t)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	ctx = injectTraceFields(ctx)

	if got := log.TraceID(ctx); got != testTraceIDHex {
		t.Errorf("trace_id = %q, want %q", got, testTraceIDHex)
	}
	if got := log.SpanID(ctx); got != testSpanIDHex {
		t.Errorf("span_id = %q, want %q", got, testSpanIDHex)
	}
}

func TestInjectTraceFieldsInvalid(t *testing.T) {
	// 无 SpanContext 的普通 context，应原样返回、不注入字段。
	ctx := injectTraceFields(context.Background())
	if log.TraceID(ctx) != "" {
		t.Error("无效 SpanContext 不应注入 trace_id")
	}
}

func TestUnaryTraceContextInterceptor(t *testing.T) {
	sc := validSpanContext(t)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	interceptor := UnaryTraceContextInterceptor()
	var gotTraceID string
	_, err := interceptor(
		ctx,
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"},
		func(c context.Context, _ any) (any, error) {
			gotTraceID = log.TraceID(c) // 下游应能读到注入的 trace_id
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("interceptor error: %v", err)
	}
	if gotTraceID != testTraceIDHex {
		t.Errorf("下游 trace_id = %q, want %q", gotTraceID, testTraceIDHex)
	}
}

func TestTraceResponseHeaderMiddleware(t *testing.T) {
	t.Run("有效 SpanContext 写入响应头", func(t *testing.T) {
		sc := validSpanContext(t)
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		handler := TraceResponseHeaderMiddleware(next)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(trace.ContextWithSpanContext(req.Context(), sc))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("X-Trace-Id"); got != testTraceIDHex {
			t.Errorf("X-Trace-Id = %q, want %q", got, testTraceIDHex)
		}
		if got := rec.Header().Get("X-Span-Id"); got != testSpanIDHex {
			t.Errorf("X-Span-Id = %q, want %q", got, testSpanIDHex)
		}
	})

	t.Run("无 SpanContext 不写响应头", func(t *testing.T) {
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		handler := TraceResponseHeaderMiddleware(next)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("X-Trace-Id"); got != "" {
			t.Errorf("无 SpanContext 不应写 X-Trace-Id, got %q", got)
		}
		if got := rec.Header().Get("X-Span-Id"); got != "" {
			t.Errorf("无 SpanContext 不应写 X-Span-Id, got %q", got)
		}
	})
}

func TestTraceAwareErrorHandler(t *testing.T) {
	marshaler := &runtime.JSONPb{}
	mux := runtime.NewServeMux()
	handler := TraceAwareErrorHandler()

	t.Run("错误响应体带 trace_id / span_id", func(t *testing.T) {
		sc := validSpanContext(t)
		ctx := trace.ContextWithSpanContext(context.Background(), sc)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/users", nil)

		handler(ctx, mux, marshaler, rec, req, status.Error(codes.Unauthenticated, "Bad authorization string"))

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("HTTP status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}

		var body errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal body error: %v, raw=%s", err, rec.Body.String())
		}
		if body.Code != int32(codes.Unauthenticated) {
			t.Errorf("code = %d, want %d", body.Code, codes.Unauthenticated)
		}
		if body.Message != "Bad authorization string" {
			t.Errorf("message = %q", body.Message)
		}
		if body.TraceID != testTraceIDHex {
			t.Errorf("trace_id = %q, want %q", body.TraceID, testTraceIDHex)
		}
		if body.SpanID != testSpanIDHex {
			t.Errorf("span_id = %q, want %q", body.SpanID, testSpanIDHex)
		}
	})

	t.Run("无 SpanContext 省略链路字段", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/users/u-1", nil)

		handler(context.Background(), mux, marshaler, rec, req, status.Error(codes.NotFound, "user not found"))

		if rec.Code != http.StatusNotFound {
			t.Errorf("HTTP status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		var body errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal body error: %v", err)
		}
		if body.TraceID != "" || body.SpanID != "" {
			t.Errorf("无 SpanContext 不应带链路字段, got trace_id=%q span_id=%q", body.TraceID, body.SpanID)
		}
	})
}

func TestGatewayOutgoingHeaderMatcher(t *testing.T) {
	if _, ok := GatewayOutgoingHeaderMatcher("x-trace-id"); ok {
		t.Error("x-trace-id 应被剔除，避免与 X-Trace-Id 响应头重复")
	}
	if _, ok := GatewayOutgoingHeaderMatcher("X-Span-Id"); ok {
		t.Error("x-span-id 应被剔除（大小写不敏感）")
	}
	if got, ok := GatewayOutgoingHeaderMatcher("x-custom"); !ok || got != runtime.MetadataHeaderPrefix+"x-custom" {
		t.Errorf("其它 metadata 应维持默认转发, got=%q ok=%v", got, ok)
	}
}
