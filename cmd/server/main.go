package main

import (
	"context"
	"errors"
	"fmt"
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
	"aura/pkg/db"
	"aura/pkg/hmac"
	"aura/pkg/jwt"
	"aura/pkg/log"
	"aura/pkg/otel"
	"aura/pkg/ratelimit"
	"aura/pkg/redis"

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

// readinessProbeTimeout 是 /readyz 单次依赖探测（DB / Redis Ping）的总超时，
// 取较短值避免探针被慢依赖拖死，超时即视为未就绪。
const readinessProbeTimeout = 2 * time.Second

func main() {
	// ---------- 1. 加载配置 ----------
	// 按 APP_ENV 选择 yaml 文件并解析；config 内部起 fsnotify 监听，config.Get() 永远拿最新快照（热更）。
	if err := config.Init(); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	defer config.Stop()
	cfg := config.Get()

	// ---------- 2. 初始化日志 ----------
	// 统一日志组件（基于 zap）；OnReload 回调让日志级别改 yaml 后立即生效，无需重启。
	logCfg := log.Config{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		Output: cfg.Log.Output,
		File: log.FileConfig{
			Path:       cfg.Log.File.Path,
			MaxSizeMB:  cfg.Log.File.MaxSizeMB,
			MaxBackups: cfg.Log.File.MaxBackups,
			MaxAgeDays: cfg.Log.File.MaxAgeDays,
			Compress:   cfg.Log.File.Compress,
		},
	}
	if err := log.Init(logCfg); err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}
	config.OnReload(func() {
		if err := log.SetLevel(config.Get().Log.Level); err != nil {
			log.Warnf("热更日志级别失败，保持原级别: %v", err)
		}
	})
	defer func() { _ = log.Sync() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ---------- 3. 装配进程级依赖：可观测性 / JWT / 数据库 / 缓存 ----------
	// 各组件细节见对应 helper。initObservability 必须先于后面的 server instrumentation 调用；
	// initInfra 返回的清理函数在进程退出时关闭连接池。
	metricsHandler := initObservability(ctx, cfg.Observability)

	warnInsecureJWTSecret(cfg.JWT.Secret)
	jwtMgr, err := jwt.NewManager(jwt.Config{
		Secret: cfg.JWT.Secret,
		Issuer: cfg.JWT.Issuer,
		TTL:    cfg.JWT.TTL(),
	})
	if err != nil {
		log.Fatalf("failed to init jwt manager: %v", err)
	}

	closeInfra := initInfra(cfg)
	defer closeInfra()

	// 单机过载保护限流器：gRPC 入口拦截器使用。enabled/rps/burst 热更生效——
	// OnReload 里调 Update 即时切换阈值（实例本身常驻，无需重启）。
	rateLimiter := ratelimit.New(cfg.RateLimit.Enabled, cfg.RateLimit.RPS, cfg.RateLimit.Burst)
	config.OnReload(func() {
		rl := config.Get().RateLimit
		rateLimiter.Update(rl.Enabled, rl.RPS, rl.Burst)
	})

	// HMAC 请求签名鉴权（面向服务间 / OpenAPI 调用）：未启用时返回 nil，HTTP 网关据此跳过签名中间件。
	hmacMgr := initHMAC(cfg.HMAC)
	// HMAC 防重放 nonce 去重存储：复用 Redis 单例（SETNX）。Redis 未启用时为 nil，
	// 中间件将退化为仅靠时间窗防重放，这里给出告警。
	nonceStore := buildHMACNonceStore(hmacMgr)

	// ---------- 4. 启动三个 server（各自独立 goroutine）----------
	// gRPC（:5568 内部高性能）/ HTTP gateway（:8080 外部 REST，经同进程 loopback 转发到 gRPC）
	// / admin（:9090 运维，承载 metrics/health/pprof，与业务端口隔离）。
	obs := cfg.Observability
	grpcServer := newGRPCServer(jwtMgr, rateLimiter)
	lis, err := net.Listen("tcp", cfg.Server.GRPCAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", cfg.Server.GRPCAddr, err)
	}

	httpServer, err := newHTTPGatewayServer(ctx, cfg.Server, hmacMgr, nonceStore, cfg.HMAC.ProtectedPrefixes)
	if err != nil {
		log.Fatalf("failed to create http gateway server: %v", err)
	}

	adminServer := admin.NewServer(admin.Options{
		Addr:              obs.AdminAddr,
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeoutSecs) * time.Second,
		MetricsPath:       obs.Metrics.Path,
		MetricsHandler:    metricsHandler,
		EnablePprof:       obs.Pprof.Enabled,
		ReadinessCheck:    newReadinessCheck(cfg),
	})

	serveAsync("gRPC server", cfg.Server.GRPCAddr, func() error { return grpcServer.Serve(lis) }, true)
	serveAsync("HTTP gateway", cfg.Server.HTTPAddr, httpServer.ListenAndServe, true)
	serveAsync("admin server (metrics/health/pprof)", obs.AdminAddr, adminServer.ListenAndServe, false)

	// ---------- 5. 等待信号并优雅关闭 ----------
	// 阻塞直到收到 SIGINT / SIGTERM，再按序关闭三个 server 并刷新可观测性缓冲。
	waitForShutdown(grpcServer, httpServer, adminServer, cfg.Server.ShutdownTimeoutSecs)
}

// initObservability 按配置装配链路追踪（OTel tracing）与指标采集（Prometheus），
// 返回 /metrics 的 handler（Metrics 未启用时为 nil）。
// 必须在创建 otelgrpc / otelhttp instrumentation 之前调用：二者会读取全局
// MeterProvider / TracerProvider，自动产出 gRPC / HTTP 的 RED 指标与 span。
func initObservability(ctx context.Context, obs config.ObservabilityConfig) http.Handler {
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

	if obs.Metrics.Enabled {
		h, err := otel.InitMetrics(serviceName)
		if err != nil {
			log.Fatalf("failed to init metrics: %v", err)
		}
		return h
	}
	return nil
}

// initInfra 按配置装配数据库 / 缓存（框架无关的 pkg 组件，连接失败直接 Fatal），
// 业务侧通过 db.Get() / redis.Get() 取全局单例。返回的清理函数应由调用方 defer，
// 在进程退出时关闭连接池。
func initInfra(cfg *config.Config) func() {
	var cleanups []func()

	if cfg.Database.Enabled {
		if err := db.Init(db.Options{
			Driver:          cfg.Database.Driver,
			DSN:             cfg.Database.DSN,
			MaxOpenConns:    cfg.Database.MaxOpenConns,
			MaxIdleConns:    cfg.Database.MaxIdleConns,
			ConnMaxLifetime: time.Duration(cfg.Database.ConnMaxLifetimeSecs) * time.Second,
		}); err != nil {
			log.Fatalf("failed to init database: %v", err)
		}
		cleanups = append(cleanups, func() { _ = db.Close() })
		log.Info("✅ database connected")
	}

	if cfg.Redis.Enabled {
		if err := redis.Init(redis.Options{
			Host:         cfg.Redis.Host,
			Port:         cfg.Redis.Port,
			Password:     cfg.Redis.Password,
			DB:           cfg.Redis.DB,
			PoolSize:     cfg.Redis.PoolSize,
			MinIdleConns: cfg.Redis.MinIdleConns,
			DialTimeout:  time.Duration(cfg.Redis.DialTimeoutSecs) * time.Second,
			ReadTimeout:  time.Duration(cfg.Redis.ReadTimeoutSecs) * time.Second,
			WriteTimeout: time.Duration(cfg.Redis.WriteTimeoutSecs) * time.Second,
			MaxRetries:   cfg.Redis.MaxRetries,
		}); err != nil {
			log.Fatalf("failed to init redis: %v", err)
		}
		cleanups = append(cleanups, func() { _ = redis.Get().Close() })
		log.Info("✅ redis connected")
	}

	// db / redis 接入 OTel：必须在 initInfra 装配完客户端、且 initObservability 已设全局
	// TracerProvider / MeterProvider 之后调用，hook 注册才有意义。未启用对应依赖时是 no-op。
	instrumentInfraWithOTel(cfg)

	return func() {
		// LIFO：后建先关，避免后续依赖（如某组件持有前者的句柄）顺序错乱。
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}

// newReadinessCheck 组装 /readyz 的就绪探针：对已启用的依赖（DB / Redis）逐个 Ping，
// 任一不可用即返回 error，让 /readyz 回 503，供 K8s / 负载均衡摘流；依赖恢复后自动复就绪。
// 当 DB 与 Redis 均未启用时返回 nil（admin 侧据此视为恒就绪）。
func newReadinessCheck(cfg *config.Config) func() error {
	if !cfg.Database.Enabled && !cfg.Redis.Enabled {
		return nil
	}
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), readinessProbeTimeout)
		defer cancel()

		if cfg.Database.Enabled {
			sqlDB := db.SQLDB()
			if sqlDB == nil {
				return errors.New("database not initialized")
			}
			if err := sqlDB.PingContext(ctx); err != nil {
				return fmt.Errorf("database: %w", err)
			}
		}

		if cfg.Redis.Enabled {
			client := redis.Get()
			if client == nil {
				return errors.New("redis not initialized")
			}
			if err := client.Ping(ctx); err != nil {
				return fmt.Errorf("redis: %w", err)
			}
		}
		return nil
	}
}

// instrumentInfraWithOTel 在 db / redis 已 Init、OTel 已设全局 Provider 之后挂 hook：
//   - tracing 启用 → 给两者各挂 trace 插件，让 SQL / Redis 调用续在请求 trace 上；
//   - metrics 启用 → 给 redis 挂 metrics hook（otelgorm 默认会上报 DBStats 指标）。
//
// 顺序：必须在 initInfra 之后（globalDB / globalClient 已就绪），且 initObservability
// 之后（TracerProvider / MeterProvider 已绑定到 OTel 全局），否则 hook 会挂到 noop。
func instrumentInfraWithOTel(cfg *config.Config) {
	if cfg.Observability.Tracing.Enabled {
		if cfg.Database.Enabled {
			db.InstrumentTracing()
		}
		if cfg.Redis.Enabled {
			redis.InstrumentTracing()
		}
	}
	if cfg.Observability.Metrics.Enabled && cfg.Redis.Enabled {
		redis.InstrumentMetrics()
	}
}

// serveAsync 在独立 goroutine 启动一个 server，统一日志与错误处理。
// 优雅关闭返回的 nil / http.ErrServerClosed 视为正常退出；其余错误：
// fatal=true 直接 log.Fatalf 终止进程（核心业务端口），否则仅 Errorf（运维端口）。
func serveAsync(name, addr string, serve func() error, fatal bool) {
	go func() {
		log.Infof("🚀 %s listening on %s", name, addr)
		err := serve()
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		if fatal {
			log.Fatalf("%s error: %v", name, err)
		}
		log.Errorf("%s error: %v", name, err)
	}()
}

// initHMAC 按配置构建 HMAC 请求签名验签器；未启用时返回 nil（HTTP 网关据此跳过签名中间件，
// 对现有 JWT 流程零影响）。启用但密钥缺失等配置错误直接 Fatal，避免"看似开启实则放行"。
func initHMAC(cfg config.HMACConfig) *hmac.Manager {
	if !cfg.Enabled {
		return nil
	}

	keys := cfg.KeyMap()
	mgr, err := hmac.NewManager(hmac.Config{Keys: keys, Skew: cfg.Skew()})
	if err != nil {
		log.Fatalf("failed to init hmac manager: %v", err)
	}

	log.Infof("✅ hmac request-signing auth enabled (%d app keys)", len(keys))
	return mgr
}

// buildHMACNonceStore 在 HMAC 启用时返回基于 Redis SETNX 的 nonce 去重存储；
// HMAC 未启用返回 nil；HMAC 启用但 Redis 未启用时返回 nil 并告警（防重放退化为仅时间窗）。
// 必须在 initInfra（Redis 初始化）之后调用。
func buildHMACNonceStore(hmacMgr *hmac.Manager) hmac.NonceStore {
	if hmacMgr == nil {
		return nil
	}

	store := interceptor.NewRedisNonceStore(redis.Get())
	if store == nil {
		log.Warn("⚠️  HMAC 已启用但 Redis 未启用：nonce 防重放退化为仅时间窗，建议开启 Redis")
	}

	return store
}

// newGRPCServer 创建 gRPC server，挂载业务 service 和拦截器链
func newGRPCServer(jwtManager *jwt.Manager, rateLimiter *ratelimit.Limiter) *grpc.Server {
	server := grpc.NewServer(
		// otelgrpc StatsHandler：依据 W3C traceparent 解析/生成 SpanContext 并注入 context，
		// 同时产出 gRPC RED 指标（依赖全局 MeterProvider）。OpenTelemetry 官方 instrumentation。
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		// 拦截器按声明顺序执行（外 → 内）：
		// recovery（兜底 panic）-> trace（把 OTel SpanContext 桥接成日志链路字段）
		// -> logging（带 trace_id 打访问日志）-> ratelimit（整机过载保护）-> auth（JWT 校验）
		// -> 业务 handler。
		// trace 必须在 logging 之前，logging 才能把 trace_id 一并打进访问日志；
		// ratelimit 放在 logging 之后（被限请求仍有访问日志/trace_id 便于观测）、auth 之前
		// （洪峰时在昂贵的 JWT 校验前就拒绝，省 CPU）。该限流经 gateway loopback 同时覆盖 HTTP 入口。
		grpc.ChainUnaryInterceptor(
			interceptor.UnaryRecoveryInterceptor(),
			interceptor.UnaryTraceContextInterceptor(),
			interceptor.UnaryLoggingInterceptor(),
			interceptor.UnaryRateLimitInterceptor(rateLimiter),
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
// 外层再包一层标准 net/http middleware（OTel、CORS、日志，以及可选的 HMAC 签名鉴权）
func newHTTPGatewayServer(
	ctx context.Context,
	srv config.ServerConfig,
	hmacMgr *hmac.Manager,
	nonces hmac.NonceStore,
	hmacPrefixes []string,
) (*http.Server, error) {
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
	// otelhttp（解析 traceparent、起 server span）-> 写回 X-Trace-Id 响应头 -> CORS
	// -> 日志 -> HMAC 签名鉴权（仅启用时）-> mux
	// HMAC 放在最内侧（紧贴 mux）：CORS 先处理 OPTIONS 预检不被签名拦截，且被拒请求仍被访问日志记录便于审计。
	var handler http.Handler = mux
	if hmacMgr != nil {
		handler = interceptor.HMACAuthMiddlewareWith(hmacMgr, nonces, hmacPrefixes, handler)
	}

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
	const (
		// insecureDevSecret 是 config 模板里附带的本地调试默认密钥，命中时给出警告。
		// 这是公开的占位字符串而非真实凭据，故抑制 gosec G101 误报。
		insecureDevSecret = "dev-only-insecure-secret-change-me" //nolint:gosec // G101: 公开占位默认值，非真实凭据

		// minSecureSecretLen 视为「足够强」的 JWT 密钥最小字节数。HS256 推荐 >=32B（256bit）。
		minSecureSecretLen = 32
	)

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
