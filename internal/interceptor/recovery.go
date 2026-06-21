package interceptor

import (
	"context"
	"runtime/debug"

	"aura/pkg/log"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UnaryRecoveryInterceptor 返回一个使用 go-grpc-middleware/v2 官方 recovery
// 拦截器的实例，捕获 handler 内部 panic 并转换为标准 gRPC Internal 错误。
//
// panic 的详细信息（包含值与堆栈）仅写入服务端日志，不回传给客户端，避免泄露
// 内部状态（SQL、连接串、敏感路径等）。
//
// 自实现已替换为社区成熟库：
//
//	github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery
func UnaryRecoveryInterceptor() grpc.UnaryServerInterceptor {
	opts := []recovery.Option{
		recovery.WithRecoveryHandlerContext(func(_ context.Context, p any) error {
			// 打印 panic 堆栈，方便定位
			log.Error(
				"gRPC handler panic",
				log.Any("panic", p),
				log.String("stack", string(debug.Stack())),
			)
			// 对外只回固定 message，详情留在日志里。
			return status.Error(codes.Internal, "internal server error")
		}),
	}
	return recovery.UnaryServerInterceptor(opts...)
}
