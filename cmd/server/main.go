package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"aura/config"
	userv1 "aura/gen/proto"
	"aura/internal/admin"
	"aura/internal/interceptor"
	"aura/internal/service"
	authjwt "aura/pkg/jwt"
	"aura/pkg/log"
	"aura/pkg/otel"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// serviceName 作为 OTel 资源属性 service.name，用于链路 / 指标归属标识。
const serviceName = "aura"

// insecureDevSecret 是 config 模板里附带的本地调试默认密钥，命中时给出警告。
// 这是公开的占位字符串而非真实凭据，故抑制 gosec G101 误报。
const insecureDevSecret = "dev-only-insecure-secret-change-me" //nolint:gosec // G101: 公开占位默认值，非真实凭据

// minSecureSecretLen 视为「足够强」的 JWT 密钥最小字节数。HS256 推荐 >=32B（256bit）。
const minSecureSecretLen = 32

func main() {
	// 加载配置（按 APP_ENV 选择文件，支持热更新）。
	if err := config.Init(); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	defer config.Stop()
	cfg := config.Get()

	// 初始化统一日志组件（基于 zap），级别/格式来自配置。
	if err := log.Init(log.Config{Level: cfg.Log.Level, Format: cfg.Log.Format}); err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}
	// 日志级别支持热更：改 yaml 后立即生效，无需重启。
	config.OnReload(func() {
		c := config.Get()
		if err := log.SetLevel(c.Log.Level); err != nil {
			log.Warnf("热更日志级别失败，保持原级别: %v", err)
		}
	})
	defer func() { _ = log.Sync() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 初始化可观测性：链路追踪（OTel）+ 指标采集（Prometheus）。
	// 指标必须先于下面创建 otelgrpc / otelhttp instrumentation，二者会读取全局
	// MeterProvider 自动产出 gRPC / HTTP 的 RED 指标。
	obs := cfg.Observability
	if obs.Tracing.Enabled {
		if err := otel.InitTracing(ctx, otel.TracingOptions{
			ServiceName: serviceName,
			Exporter:    obs.Tracing.Exporter,
			Endpoint:    obs.Tracing.Endpoint,
			Insecure:    obs.Tracing.Insecure,
			SampleRatio: obs.Tracing.SampleRatio,
		}); err != nil {
			log.Fatalf("failed to init tracing: %v", err)
		}
	}

	var metricsHandler http.Handler
	if obs.Metrics.Enabled {
		h, err := otel.InitMetrics(serviceName)
		if err != nil {
			log.Fatalf("failed to init metrics: %v", err)
		}
		metricsHandler = h
	}

	// JWT 管理器：密钥、issuer、有效期全部来自配置文件。
	// 生产环境请在 yaml 中用 ${JWT_SECRET} 从环境变量注入，切勿使用调试默认值。
	warnInsecureJWTSecret(cfg.JWT.Secret)
	jwtManager, err := authjwt.NewManager(authjwt.Config{
		Secret: cfg.JWT.Secret,
		Issuer: cfg.JWT.Issuer,
		TTL:    cfg.JWT.TTL(),
	})
	if err != nil {
		log.Fatalf("failed to init jwt manager: %v", err)
	}

	// ---------- 1. 启动 gRPC Server ----------
	grpcServer := newGRPCServer(jwtManager)

	lis, err := net.Listen("tcp", cfg.Server.GRPCAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", cfg.Server.GRPCAddr, err)
	}

	go func() {
		log.Infof("🚀 gRPC server listening on %s", cfg.Server.GRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("grpc server error: %v", err)
		}
	}()

	// ---------- 2. 启动 HTTP Gateway Server ----------
	// grpc-gateway 的标准做法：起一个 grpc.ClientConn 连到本地的 gRPC server，
	// 把 HTTP 请求"翻译"成 gRPC 请求后通过这个连接转发过去。
	// 因为是同进程内 loopback 调用，延迟可以忽略不计。
	httpServer, err := newHTTPGatewayServer(ctx, cfg.Server)
	if err != nil {
		log.Fatalf("failed to create http gateway server: %v", err)
	}

	go func() {
		log.Infof("🚀 HTTP gateway listening on %s", cfg.Server.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	// ---------- 3. 启动运维/可观测性 Server ----------
	// 独立端口承载 /metrics、/healthz、/readyz、/debug/pprof，与业务端口隔离。
	adminServer := admin.NewServer(admin.Options{
		Addr:              obs.AdminAddr,
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeoutSecs) * time.Second,
		MetricsPath:       obs.Metrics.Path,
		MetricsHandler:    metricsHandler,
		EnablePprof:       obs.Pprof.Enabled,
	})

	go func() {
		log.Infof("🚀 admin server listening on %s (metrics/health/pprof)", obs.AdminAddr)
		if err := adminServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("admin server error: %v", err)
		}
	}()

	// ---------- 4. 优雅关闭 ----------
	waitForShutdown(grpcServer, httpServer, adminServer, cfg.Server.ShutdownTimeoutSecs)
}

// newGRPCServer 创建 gRPC server，挂载业务 service 和拦截器链
func newGRPCServer(jwtManager *authjwt.Manager) *grpc.Server {
	server := grpc.NewServer(
		// otelgrpc StatsHandler：依据 W3C traceparent 解析/生成 SpanContext 并注入 context，
		// 同时产出 gRPC RED 指标（依赖全局 MeterProvider）。OpenTelemetry 官方 instrumentation。
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		// 拦截器按声明顺序执行（外 → 内）：
		// recovery（兜底 panic）-> trace（把 OTel SpanContext 桥接成日志链路字段）
		// -> logging（带 trace_id 打访问日志）-> auth（JWT 校验）-> 业务 handler。
		// trace 必须在 logging 之前，logging 才能把 trace_id 一并打进访问日志。
		grpc.ChainUnaryInterceptor(
			interceptor.UnaryRecoveryInterceptor(),
			interceptor.UnaryTraceContextInterceptor(),
			interceptor.UnaryLoggingInterceptor(),
			interceptor.UnaryAuthInterceptor(jwtManager),
		),
	)

	userv1.RegisterUserServiceServer(server, service.NewUserServer())

	// gRPC 标准健康检查服务（grpc.health.v1.Health），供 K8s gRPC 探针 /
	// grpc_health_probe / 负载均衡使用。空 service 名代表整体服务状态。
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(server, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	return server
}

// newHTTPGatewayServer 创建 HTTP server：
// grpc-gateway 生成的 mux 负责把 /v1/users 这类 REST 请求翻译成 gRPC 调用，
// 外层再包一层标准 net/http middleware（OTel、CORS、日志）
func newHTTPGatewayServer(ctx context.Context, srv config.ServerConfig) (*http.Server, error) {
	mux := runtime.NewServeMux(
		// 自定义 JSON 序列化风格：
		// - EmitUnpopulated: 零值字段也输出，前端联调更直观（不用猜字段是不是被裁掉了）
		// - UseProtoNames: 用 proto 里的 snake_case（created_at）而不是默认 camelCase（createdAt）
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				EmitUnpopulated: true,
				UseProtoNames:   true,
			},
			UnmarshalOptions: protojson.UnmarshalOptions{
				DiscardUnknown: true,
			},
		}),
		// 错误响应体里注入 trace_id / span_id，方便调用方报错时直接拿到链路标识排障。
		runtime.WithErrorHandler(interceptor.TraceAwareErrorHandler()),
		// 链路标识已由 TraceResponseHeader 中间件以干净的 X-Trace-Id / X-Span-Id 暴露，
		// 这里剔除 loopback gRPC metadata 里的同名项，避免 HTTP 响应出现冗余的 Grpc-Metadata-*。
		runtime.WithOutgoingHeaderMatcher(interceptor.GatewayOutgoingHeaderMatcher),
	)

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()), // 同机 loopback，demo 用明文；生产建议至少用 mTLS
		// otelgrpc 客户端 instrumentation：把 HTTP 入口的 SpanContext 通过 traceparent
		// metadata 注入 loopback gRPC 调用，使 HTTP 与 gRPC 落在同一条 trace 上。
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	}

	conn, err := grpc.NewClient(srv.GRPCAddr, dialOpts...)
	if err != nil {
		return nil, err
	}

	if err := userv1.RegisterUserServiceHandler(ctx, mux, conn); err != nil {
		return nil, err
	}

	// HTTP 端 middleware 链（外 → 内）：
	// otelhttp（解析 traceparent、起 server span）-> 写回 X-Trace-Id 响应头 -> CORS -> 日志 -> mux
	var handler http.Handler = mux
	handler = interceptor.HTTPLoggingMiddleware(handler)
	handler = interceptor.CORSMiddlewareWith(corsOptionsFrom(srv.CORS), handler)
	handler = interceptor.TraceResponseHeaderMiddleware(handler)
	handler = otelhttp.NewHandler(handler, "http.gateway")

	return &http.Server{
		Addr:    srv.HTTPAddr,
		Handler: handler,
		// 设置读取请求头超时，防止 Slowloris 慢速攻击（gosec G112）
		ReadHeaderTimeout: time.Duration(srv.ReadHeaderTimeoutSecs) * time.Second,
	}, nil
}

// corsOptionsFrom 把 config 中的 CORSConfig 映射成中间件需要的 CORSOptions，
// 并对「AllowedOrigins=["*"] 且 AllowCredentials=true」这种不合规组合 fail-fast。
func corsOptionsFrom(c config.CORSConfig) interceptor.CORSOptions {
	if c.AllowCredentials && slices.Contains(c.AllowedOrigins, "*") {
		log.Fatal("cors: allowed_origins=['*'] 不能与 allow_credentials=true 同时使用（违反 CORS 规范）")
	}
	return interceptor.CORSOptions{
		AllowedOrigins:   c.AllowedOrigins,
		AllowedMethods:   c.AllowedMethods,
		AllowedHeaders:   c.AllowedHeaders,
		AllowCredentials: c.AllowCredentials,
	}
}

// warnInsecureJWTSecret 在密钥疑似不安全时打印告警，覆盖三类典型场景：
//   - 命中 config 模板内的占位默认值；
//   - 长度过短（HS256 推荐 >=32B，OWASP 也建议至少 256bit 熵）；
//   - 未注入（${JWT_SECRET} 未展开导致空串）—— 由 NewManager 报错，不在此处理。
func warnInsecureJWTSecret(secret string) {
	switch {
	case secret == insecureDevSecret:
		log.Warn("⚠️  正在使用 config 中的本地调试 JWT 密钥，切勿用于生产")
	case len(secret) < minSecureSecretLen:
		log.Warnf("⚠️  JWT 密钥长度仅 %d 字节（建议 >= %d），生产环境请使用强随机密钥",
			len(secret), minSecureSecretLen)
	}
}

func waitForShutdown(grpcServer *grpc.Server, httpServer, adminServer *http.Server, shutdownTimeoutSecs int) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("⏳ shutting down servers gracefully...")

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(shutdownTimeoutSecs)*time.Second,
	)
	defer cancel()

	_ = httpServer.Shutdown(shutdownCtx)
	_ = adminServer.Shutdown(shutdownCtx)
	grpcServer.GracefulStop()
	// 关闭可观测性：刷新缓冲中的 span / 指标后释放。
	_ = otel.ShutdownMetrics(shutdownCtx)
	_ = otel.ShutdownTracing(shutdownCtx)

	log.Info("✅ servers stopped")
}
