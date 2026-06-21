package interceptor

import (
	"context"
	"io"
	"net/http"
	"strings"

	"aura/pkg/log"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/spf13/cast"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// 链路信息回传给请求方时使用的固定名称：
//   - HTTP 响应头用 X-Trace-Id / X-Span-Id（成功 / 失败都会带上）。
//   - gRPC 响应 header metadata 用小写 key（gRPC metadata 规范要求小写）。
const (
	httpTraceHeader = "X-Trace-Id"
	httpSpanHeader  = "X-Span-Id"
	grpcTraceKey    = "x-trace-id"
	grpcSpanKey     = "x-span-id"
)

// UnaryTraceContextInterceptor 把 OpenTelemetry 的 SpanContext 桥接到 pkg/log 的链路字段，
// 使下游用 log.InfoContext 打印时自动带上 trace_id / span_id；同时把 trace_id / span_id
// 写进 gRPC 响应 header metadata，让原生 gRPC 调用方（grpcurl / 内部服务）无论成功还是失败
// 都能拿到链路标识，便于排障定位。
//
// 链路的解析（W3C traceparent）、TraceID/SpanID 生成、采样与跨进程传播，均由成熟的
// OpenTelemetry 生态完成：gRPC 入口挂 otelgrpc.NewServerHandler() 后，SpanContext 已注入
// context，本拦截器只做"读取 → 写入日志字段 / 响应 metadata"这一层薄桥接，不重复造轮子。
//
// 应挂在拦截器链中 logging 之前，确保访问日志也能带上 trace_id（见 cmd/server/main.go）。
func UnaryTraceContextInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		_ *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		ctx = injectTraceFields(ctx)
		// 在调用 handler 前设置 header metadata：即使 handler（或后续 auth 拦截器）返回
		// 错误，已写入的 header 仍会随响应发回客户端。
		setGRPCTraceHeader(ctx)
		return handler(ctx, req)
	}
}

// TraceResponseHeaderMiddleware 把当前请求的 trace_id / span_id 写入 HTTP 响应头
// X-Trace-Id / X-Span-Id，无论成功响应还是错误响应都会带上，便于客户端 / 中间网关
// 串联日志。需挂在 otelhttp 之内（otelhttp 已把 SpanContext 注入 r.Context()）。
func TraceResponseHeaderMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			w.Header().Set(httpTraceHeader, sc.TraceID().String())
			w.Header().Set(httpSpanHeader, sc.SpanID().String())
		}
		next.ServeHTTP(w, r)
	})
}

// TraceAwareErrorHandler 返回一个 grpc-gateway 错误处理器：在标准错误响应体
// （code / message）基础上额外注入 trace_id / span_id，使 HTTP 调用方在接口报错时
// 能直接从响应体里拿到链路标识上报排障，而不必去翻服务端日志。
//
// 错误响应示例：
//
//	{"code":16,"message":"Bad authorization string","trace_id":"...","span_id":"..."}
func TraceAwareErrorHandler() runtime.ErrorHandlerFunc {
	const fallback = `{"code":13,"message":"failed to marshal error message"}`

	return func(
		ctx context.Context,
		_ *runtime.ServeMux,
		m runtime.Marshaler,
		w http.ResponseWriter,
		_ *http.Request, err error,
	) {
		s := status.Convert(err)

		body := errorBody{
			Code:    cast.ToInt32(s.Code()),
			Message: s.Message(),
		}
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			body.TraceID = sc.TraceID().String()
			body.SpanID = sc.SpanID().String()
		}

		w.Header().Del("Trailer")
		w.Header().Del("Transfer-Encoding")
		w.Header().Set("Content-Type", m.ContentType(body))
		// 与 grpc-gateway 默认行为保持一致：鉴权失败时回写 WWW-Authenticate。
		if s.Code() == codes.Unauthenticated {
			w.Header().Set("WWW-Authenticate", s.Message())
		}

		buf, merr := m.Marshal(body)
		if merr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, fallback)
			return
		}

		w.WriteHeader(runtime.HTTPStatusFromCode(s.Code()))
		_, _ = w.Write(buf)
	}
}

// GatewayOutgoingHeaderMatcher 控制 gRPC 响应 metadata 如何映射成 HTTP 响应头。
// 链路相关的 x-trace-id / x-span-id 已由 TraceResponseHeaderMiddleware 以干净的
// X-Trace-Id / X-Span-Id 暴露，这里将其从 loopback gRPC metadata 中剔除，避免 HTTP
// 响应里再出现一份冗余的 Grpc-Metadata-X-Trace-Id；其余 metadata 维持默认转发行为。
func GatewayOutgoingHeaderMatcher(key string) (string, bool) {
	switch strings.ToLower(key) {
	case grpcTraceKey, grpcSpanKey:
		return "", false
	default:
		return runtime.MetadataHeaderPrefix + key, true
	}
}

// errorBody 是带链路标识的 HTTP 错误响应体结构。
type errorBody struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
}

// injectTraceFields 从 OTel SpanContext 读取 trace_id / span_id 并写入 pkg/log 链路字段；
// SpanContext 无效（如未经 otelgrpc 的极端情况）时原样返回。
func injectTraceFields(ctx context.Context) context.Context {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ctx
	}
	return log.WithFields(
		ctx,
		log.String(log.KeyTraceID, sc.TraceID().String()),
		log.String(log.KeySpanID, sc.SpanID().String()),
	)
}

// setGRPCTraceHeader 把 trace_id / span_id 写入 gRPC 响应 header metadata；
// SpanContext 无效时不做任何事。
func setGRPCTraceHeader(ctx context.Context) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return
	}
	_ = grpc.SetHeader(ctx, metadata.Pairs(
		grpcTraceKey, sc.TraceID().String(),
		grpcSpanKey, sc.SpanID().String(),
	))
}
