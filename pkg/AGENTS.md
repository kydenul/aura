# pkg · L2

框架无关的可复用组件。所有包**都不反向 import 业务 / config 包**（避免循环依赖），由 `cmd/server/main.go` 在启动期用 `config` 的值完成装配。可被任意项目直接拷用。

## jwt/ — JWT 签发与校验

封装 `golang-jwt/jwt/v5`，与具体框架解耦，gRPC 拦截器 / HTTP 中间件 / 业务代码共用同一个 `Manager`。

| 文件         | 导出                                                                     | 说明                                                  |
| ------------ | ------------------------------------------------------------------------ | ----------------------------------------------------- |
| `jwt.go`     | `Manager`、`NewManager(Config)`、`Generate`、`Parse`、`Claims`、`ErrXxx` | HS256 签发 / 校验；`Manager` 创建后并发安全，可作单例 |
| `context.go` | `NewContext`、`FromContext`                                              | Claims 与 context 互转（私有 ctxKey，避免冲突）       |

安全要点：

- 解析时 `WithValidMethods` 强制限定算法，杜绝 `alg=none` 与 RS/HS 混淆攻击。
- `WithExpirationRequired` 强制要求 `exp`；配置了 `Issuer` 则强制校验 `iss`。
- 失败错误可用 `errors.Is` 与 `ErrTokenExpired` / `ErrInvalidToken` 精确比对；包装格式 `%w: %v` 同时保留底层库的诊断信息。
- `Secret` 必填，生产务必从环境变量 / 密钥管理服务注入，勿硬编码。HS256 推荐 >= 32 字节（256bit）；`cmd/server` 启动期会对命中调试占位值或长度过短的密钥打印告警。

## log/ — 结构化日志

封装 `go.uber.org/zap`，单例 + 包级便捷函数，import 即用，无需层层传 logger。

| 文件         | 导出                                                                                                                    | 说明                                                                                                                 |
| ------------ | ----------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `log.go`     | `Init`、`SetLevel`、`Info/Warn/Error/...`、`Infof/...`、字段构造器（`String`/`Int`/`Any`/...）、`L()`/`S()`、`Sync`     | `format` 支持 `text`（控制台彩色）/ `json`（生产采集）；级别用 `AtomicLevel` 持有，`SetLevel` 不重建 logger 即可热更 |
| `context.go` | `WithFields`、`WithTraceID/SpanID/RequestID`、`InfoContext/...`、`InfofContext/...`、`Fields(ctx)`、`KeyTraceID` 等常量 | 把链路字段挂到 context，`*Context` 系列函数打印时自动带出，实现日志链路追踪                                          |

要点：

- `init()` 已提供安全默认 logger（text/info），早于 `Init` 的日志调用不会空指针。
- 级别热更：配合 `config.OnReload` → `log.SetLevel(...)`，改 yaml 立即生效。
- 链路字段 key 全项目统一：`trace_id` / `span_id` / `request_id`（`interceptor/trace.go` 写入，`service` 用 `InfoContext` 读出）。

## otel/ — 链路追踪 + 指标

封装 OpenTelemetry，链路解析 / TraceID 生成 / 采样 / 跨进程传播 / 指标采集全交给成熟生态，业务无需自研。

| 文件         | 导出                                                                                                      | 说明                                                                                      |
| ------------ | --------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| `tracing.go` | `InitTracing(ctx, TracingOptions)`、`ShutdownTracing(ctx)`、`TracingOptions`、`ExporterNone/ExporterOTLP` | 装配全局 `TracerProvider` + W3C / Baggage 传播器；exporter 可选 none / otlp，采样比例可配 |
| `metrics.go` | `InitMetrics(serviceName) (http.Handler, error)`、`ShutdownMetrics(ctx)`                                  | 装配全局 `MeterProvider`（Prometheus 拉取式）+ Go runtime 指标，返回 `/metrics` handler   |

要点：

- **trace_id 始终可用**：采样只决定 span 是否上报，无论采样与否 SpanContext 都带有效 TraceID/SpanID，日志的 `trace_id` 稳定可用。`SampleRatio` >=1 全采样、<=0 不采样、其余按 TraceID 比例。
- **span 上报可配**：`Exporter=none`（默认，零外部依赖）只在进程内传播；`Exporter=otlp` 时通过 `BatchSpanProcessor` 异步上报到 collector（Jaeger / Tempo / 伽利略 / OTLP）。
- **指标零埋点**：`InitMetrics` 必须在创建 otelgrpc / otelhttp instrumentation **之前**调用（设置全局 MeterProvider），之后 gRPC / HTTP 的 RED 指标（请求量 / 错误率 / 耗时）自动产出；runtime 指标由 `contrib/instrumentation/runtime` 采集。
- 入口 instrumentation 在 `cmd/server/main.go`：gRPC 挂 `otelgrpc.NewServerHandler()`，HTTP 挂 `otelhttp.NewHandler(...)`；二者通过 `traceparent` 落在同一条 trace。
- `/metrics`、健康检查、pprof 端点统一由 [`internal/admin`](../internal/AGENTS.md) 挂在独立运维端口暴露。

## db/ — MySQL / PostgreSQL（GORM 单例）

封装 `gorm.io/gorm`，按 `Options.Driver` 选 `gorm.io/driver/mysql` 或 `gorm.io/driver/postgres`，单例 + 包级便捷函数。**按需启用**：`config.Database.Enabled=true` 时 `main` 才调 `db.Init` 建连接池（Ping 校验失败直接 Fatal），否则进程不连库。

| 文件    | 导出                                                                                                                                              | 说明                                                                                                                                                                                                                |
| ------- | ------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `db.go` | `Init(Options)`、`Get() *gorm.DB`、`SQLDB() *sql.DB`、`Close()`、`InstrumentTracing()`、`InjectTestDB`、`Options`、`DriverMySQL`/`DriverPostgres` | `Init` 按 `Driver` 选 dialector 建连接池（MaxOpen/MaxIdle/Lifetime 可配，<=0 取默认）；`Get` 取全局实例供业务查询；`Close` 由 `main` defer 优雅释放；`InstrumentTracing` 挂 otelgorm 插件；`InjectTestDB` 供单测替换 |

要点：

- 切换数据库只改配置：`database.driver: mysql | postgres`（留空按 mysql）+ 对应格式 DSN，**代码零改动**；驱动非法启动期报错。
- DSN 格式：mysql 形如 `user:pass@tcp(host:3306)/db?charset=utf8mb4&parseTime=true&loc=Local`；postgres 形如 `host=h user=u password=p dbname=d port=5432 sslmode=disable TimeZone=Asia/Shanghai`（亦支持 `postgres://...` URL）。生产用 `${DATABASE_DSN}` 从环境变量注入。
- GORM 自带 logger 设为 `Silent`（日志走统一 zap 体系）；`NowFunc` 用本地时区。
- **OTel 续在请求 trace 上**：`tracing.enabled=true` 时 `main` 自动调用 `db.InstrumentTracing()`（依赖 `uptrace/otelgorm`）。业务侧务必用 `db.Get().WithContext(ctx).Find(...)` 显式传 ctx，否则 SQL span 是孤立的，串不进入口 trace。
- `/readyz` 通过 `db.SQLDB().PingContext(ctx)` 探活，2s 超时（`main` 中 `readinessProbeTimeout`）。

## redis/ — Redis（go-redis v9）

封装 `github.com/redis/go-redis/v9`，`Client` 屏蔽底层类型。**按需启用**：`config.Redis.Enabled=true` 时 `main` 才调 `redis.Init`（带重试 Ping，兼容 Codis / 腾讯云 Proxy 主备切换）。

| 文件           | 导出                                                                                                                                                                                            | 说明                                                                                                                                                                |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `redis.go`     | `Init(Options)`、`Get() *Client`、`IsInitialized`、`NewClient`、`NewClientFromConn`、`InstrumentTracing()`、`InstrumentMetrics()`、`Client.{Set/Get/Del/SetNX/Incr/Hash/Set/ZSet/Pub/Sub...}`、`Nil`、`Options` | 单例 + 通用操作集（String/Hash/Set/ZSet/原子/Pub-Sub）；`Nil` 等价 `redis.Nil` 用于判 key 不存在；`Instrument*` 挂 redisotel hook；`NewClientFromConn` 供单测注入   |
| `ratelimit.go` | `NewSlidingWindowLimiter(client, keyPrefix, limit, window)`、`SlidingWindowLimiter.Allow`、`ErrRateLimitNotReady`                                                                                | 基于 ZSet + Lua 的精确滑动窗口限流，Lua 由 `redis.NewScript` 缓存（自动 EVALSHA）；ZSet member 由 `atomic.Uint64` 单调序列保证不去重；`client=nil` 返回 `ErrRateLimitNotReady` |

要点：

- 连接参数（PoolSize/MinIdleConns/各类超时/MaxRetries）显式可配；`ConnMaxIdleTime`/`ConnMaxLifetime` 在包内常量固定（规避 Proxy 默认 600s 空闲断连，业务无需关心）。
- `Del` / `Exists` 接受任意 keys，**一次 RTT**完成；集群分片场景由调用方在 key 模板里加 hash tag（如 `user:{uid}:profile`）确保同 slot，避免 CROSSSLOT。
- Redis key 命名按业务维度前缀、TTL 必须显式，新增 key 在所属业务包集中定义常量。
- **OTel 集成**：`tracing.enabled=true` 时 `main` 调用 `redis.InstrumentTracing()`，每条命令一个 span；`metrics.enabled=true` 时调用 `redis.InstrumentMetrics()`，pool & 命令耗时并入 `/metrics`。
- `/readyz` 通过 `redis.Get().Ping(ctx)` 探活，与 DB 共享 2s 超时。

## 组件协作

```
config(值) ──Init──> log / jwt / otel / db / redis(装配)
                         │
otel 全局 Provider ──InstrumentTracing/Metrics──> db / redis(挂 hook)
                         │
interceptor/trace ──写──> log context(trace_id/span_id)
                         │
service ────WithContext(ctx)─────> db / redis 调用续在父 trace 上
service ────InfoContext─读─> 自动带链路字段输出
```

## 常见入口

| 想做什么          | 改哪里                                                                                                     |
| ----------------- | ---------------------------------------------------------------------------------------------------------- |
| 签发 / 校验 token | `jwt.Manager.Generate` / `Parse`；取 Claims 用 `jwt.FromContext(ctx)`                                      |
| 加业务角色鉴权    | `jwt.Claims.HasRole(...)`（粗粒度）+ 在 handler / 拦截器判断                                               |
| 打带链路的日志    | `log.InfoContext(ctx, msg, log.String(...))`                                                               |
| 加一个链路字段    | `log.WithFields(ctx, log.String("key", v))`；公共 key 加到 `context.go` 常量                               |
| 上报 trace 到后端 | 配置 `observability.tracing.exporter: otlp` + `endpoint`（见 `config/AGENTS.md`），无需改代码              |
| 调采样率          | 配置 `observability.tracing.sample_ratio`（0~1）                                                           |
| 加自定义业务指标  | `otel.GetMeterProvider().Meter("...")` 创建 counter / histogram，自动并入 `/metrics`                       |
| 用数据库          | 配置 `database.enabled: true` + `dsn`；业务侧 `db.Get().WithContext(ctx)` 拿 `*gorm.DB`（带 ctx 才能续 trace） |
| 用 Redis          | 配置 `redis.enabled: true` + 连接参数；业务侧 `redis.Get()` 拿 `*Client`                                   |
| 加接口限流        | `redis.NewSlidingWindowLimiter(redis.Get(), "ratelimit:xxx", limit, window)` 后在拦截器/handler 调 `Allow` |
