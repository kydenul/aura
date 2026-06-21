// 本文件为 log 组件提供「上下文链路字段」能力：
// 把 trace_id / request_id 等贯穿一次调用的字段挂到 context 上，之后用
// InfoContext / InfofContext 等带 Context 的函数打印时，会自动把这些字段一并输出，
// 从而实现日志链路追踪（无需在每个调用点手动重复传 trace_id）。
//
// 典型用法：
//
//	// 入口（如 gRPC 拦截器）注入：
//	ctx = log.WithTraceID(ctx, traceID)
//	ctx = log.WithRequestID(ctx, requestID)
//
//	// 业务任意层级打印，自动带上 trace_id / request_id：
//	log.InfoContext(ctx, "user created", log.String("uid", id))
//	log.InfofContext(ctx, "user %s created", id)
package log

import (
	"context"
	"fmt"
)

// 约定的链路字段 key，日志与跨服务透传均使用同一组名称。
const (
	KeyTraceID   = "trace_id"   // 一次请求/调用链的唯一标识（对应 OTel TraceID）
	KeySpanID    = "span_id"    // 当前 span 标识（对应 OTel SpanID）
	KeyRequestID = "request_id" // 单次请求标识（非 OTel 概念，供自定义场景使用）
)

// ctxFieldsKey 是私有类型的 context key，避免与其它包冲突。
type ctxFieldsKey struct{}

// WithFields 返回携带指定字段的新 context；与已存在的字段合并，同名 key 以新值覆盖。
// 这是注入任意链路字段的通用入口，trace_id / request_id 只是其常见特例。
func WithFields(ctx context.Context, fields ...Field) context.Context {
	if len(fields) == 0 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}

	existing := fieldsFromContext(ctx)
	merged := make([]Field, len(existing), len(existing)+len(fields))
	copy(merged, existing)

	for _, f := range fields {
		replaced := false
		for i := range merged {
			if merged[i].Key == f.Key {
				merged[i] = f // 同名覆盖，避免重复 key
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, f)
		}
	}
	return context.WithValue(ctx, ctxFieldsKey{}, merged)
}

// WithTraceID 把 trace_id 挂到 context（trace_id 为空则原样返回，不污染上下文）。
func WithTraceID(ctx context.Context, traceID string) context.Context {
	if traceID == "" {
		return ctx
	}
	return WithFields(ctx, String(KeyTraceID, traceID))
}

// WithSpanID 把 span_id 挂到 context（span_id 为空则原样返回）。
func WithSpanID(ctx context.Context, spanID string) context.Context {
	if spanID == "" {
		return ctx
	}
	return WithFields(ctx, String(KeySpanID, spanID))
}

// WithRequestID 把 request_id 挂到 context（request_id 为空则原样返回）。
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if requestID == "" {
		return ctx
	}
	return WithFields(ctx, String(KeyRequestID, requestID))
}

// Fields 返回 context 上已挂载的链路字段副本，便于其它组件（如访问日志适配器）复用。
func Fields(ctx context.Context) []Field {
	src := fieldsFromContext(ctx)
	if len(src) == 0 {
		return nil
	}
	out := make([]Field, len(src))
	copy(out, src)
	return out
}

// TraceID 便捷读取 context 上的 trace_id；不存在返回空串。
func TraceID(ctx context.Context) string { return stringField(ctx, KeyTraceID) }

// SpanID 便捷读取 context 上的 span_id；不存在返回空串。
func SpanID(ctx context.Context) string { return stringField(ctx, KeySpanID) }

// RequestID 便捷读取 context 上的 request_id；不存在返回空串。
func RequestID(ctx context.Context) string { return stringField(ctx, KeyRequestID) }

func fieldsFromContext(ctx context.Context) []Field {
	if ctx == nil {
		return nil
	}
	fs, _ := ctx.Value(ctxFieldsKey{}).([]Field)
	return fs
}

func stringField(ctx context.Context, key string) string {
	for _, f := range fieldsFromContext(ctx) {
		if f.Key == key {
			return f.String
		}
	}
	return ""
}

// mergeCtx 把 context 链路字段（trace_id / span_id 等观测信息）追加到本次调用
// 显式传入的业务字段之后，使日志里「有价值的业务数据在前、观测信息在后」，更易阅读。
func mergeCtx(ctx context.Context, fields []Field) []Field {
	cf := fieldsFromContext(ctx)
	if len(cf) == 0 {
		return fields
	}
	out := make([]Field, 0, len(fields)+len(cf))
	out = append(out, fields...)
	out = append(out, cf...)
	return out
}

// ── 带 Context 的结构化便捷函数（自动附带 context 链路字段）──────────────────

// DebugContext 记录 debug 日志，并附带 context 上的链路字段。
func DebugContext(ctx context.Context, msg string, fields ...Field) {
	logger().Debug(msg, mergeCtx(ctx, fields)...)
}

// InfoContext 记录 info 日志，并附带 context 上的链路字段。
func InfoContext(ctx context.Context, msg string, fields ...Field) {
	logger().Info(msg, mergeCtx(ctx, fields)...)
}

// WarnContext 记录 warn 日志，并附带 context 上的链路字段。
func WarnContext(ctx context.Context, msg string, fields ...Field) {
	logger().Warn(msg, mergeCtx(ctx, fields)...)
}

// ErrorContext 记录 error 日志，并附带 context 上的链路字段。
func ErrorContext(ctx context.Context, msg string, fields ...Field) {
	logger().Error(msg, mergeCtx(ctx, fields)...)
}

// ── 带 Context 的格式化便捷函数（printf 风格 + context 链路字段）─────────────
//
// 命名采用 <Level>fContext 约定（与 slog 的 *Context 系列一致），故对
// goprintffuncname 这一「printf 函数名须以 f 结尾」的启发式规则做局部豁免。

// DebugfContext 以 printf 风格记录 debug 日志，并附带 context 上的链路字段。
//
//nolint:goprintffuncname // 命名遵循 <Level>fContext 约定
func DebugfContext(ctx context.Context, format string, args ...any) {
	logger().Debug(fmt.Sprintf(format, args...), fieldsFromContext(ctx)...)
}

// InfofContext 以 printf 风格记录 info 日志，并附带 context 上的链路字段。
//
//nolint:goprintffuncname // 命名遵循 <Level>fContext 约定
func InfofContext(ctx context.Context, format string, args ...any) {
	logger().Info(fmt.Sprintf(format, args...), fieldsFromContext(ctx)...)
}

// WarnfContext 以 printf 风格记录 warn 日志，并附带 context 上的链路字段。
//
//nolint:goprintffuncname // 命名遵循 <Level>fContext 约定
func WarnfContext(ctx context.Context, format string, args ...any) {
	logger().Warn(fmt.Sprintf(format, args...), fieldsFromContext(ctx)...)
}

// ErrorfContext 以 printf 风格记录 error 日志，并附带 context 上的链路字段。
//
//nolint:goprintffuncname // 命名遵循 <Level>fContext 约定
func ErrorfContext(ctx context.Context, format string, args ...any) {
	logger().Error(fmt.Sprintf(format, args...), fieldsFromContext(ctx)...)
}
