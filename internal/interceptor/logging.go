package interceptor

import (
	"context"
	"fmt"

	"aura/pkg/log"

	grpclogging "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"google.golang.org/grpc"
)

// UnaryLoggingInterceptor 返回一个使用 go-grpc-middleware/v2 官方 logging
// 拦截器的实例，记录每个 gRPC 请求的方法名、耗时、状态等。
//
// 拦截器框架来自社区成熟库：
//
//	github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging
//
// 底层日志落地统一走 pkg/log（zap），级别/格式由 config 驱动并支持热更。
func UnaryLoggingInterceptor() grpc.UnaryServerInterceptor {
	return grpclogging.UnaryServerInterceptor(
		zapLogAdapter(),
		// 只在请求结束时打印一行汇总日志，避免太啰嗦；
		// 如需打印请求/响应 payload，可加 logging.WithLogOnEvents(StartCall, PayloadReceived, PayloadSent, FinishCall)
		grpclogging.WithLogOnEvents(grpclogging.FinishCall),
	)
}

// zapLogAdapter 把 pkg/log 适配成 go-grpc-middleware 要求的 logging.Logger 接口：
// 将其 key/value 交替排列的扁平字段转换为 zap 结构化字段，合并 context 上的链路字段
// （trace_id / request_id 等），再按级别落地，从而让访问日志也带上链路信息。
func zapLogAdapter() grpclogging.Logger {
	return grpclogging.LoggerFunc(func(ctx context.Context, lvl grpclogging.Level, msg string, kv ...any) {
		// 先放本次调用的业务字段，再追加 context 链路字段，
		// 保证有价值的数据在前、trace_id / span_id 等观测信息在后，便于阅读。
		ctxFields := log.Fields(ctx)
		fields := make([]log.Field, 0, len(kv)/2+len(ctxFields))
		for i := 0; i+1 < len(kv); i += 2 {
			key, ok := kv[i].(string)
			if !ok {
				key = fmt.Sprintf("%v", kv[i])
			}
			fields = append(fields, log.Any(key, kv[i+1]))
		}
		fields = append(fields, ctxFields...)

		switch lvl {
		case grpclogging.LevelDebug:
			log.Debug(msg, fields...)
		case grpclogging.LevelInfo:
			log.Info(msg, fields...)
		case grpclogging.LevelWarn:
			log.Warn(msg, fields...)
		case grpclogging.LevelError:
			log.Error(msg, fields...)
		default:
			log.Info(msg, fields...)
		}
	})
}
