# internal · L2

业务实现。三部分职责清晰：

- **`service/`** — gRPC handler，实现 `proto` 生成的 `XxxServiceServer` 接口；纯业务逻辑。
- **`interceptor/`** — 横切关注点：gRPC Unary 拦截器 + HTTP 中间件，由 `cmd/server/main.go` 装配成链。
- **`admin/`** — 运维/可观测性 HTTP 服务：`/metrics`、`/healthz`、`/readyz`、`/debug/pprof`，挂独立端口。

> `internal/` 是 Go 的私有包，外部模块不可导入。

## service/ — gRPC handler

| 文件      | 实现接口                   | 说明                                                                                                 |
| --------- | -------------------------- | ---------------------------------------------------------------------------------------------------- |
| `user.go` | `userv1.UserServiceServer` | 用户 CRUD。当前用**内存 map + RWMutex** 模拟存储，方便直接跑；真实项目在此注入 DB / Redis 等依赖替换 |

约定：

- 入参校验失败用 `status.Error(codes.InvalidArgument, ...)`，未找到用 `codes.NotFound`，遵循 gRPC 标准错误码（gateway 会自动映射成对应 HTTP 状态码）。
- 业务日志用 `log.InfoContext(ctx, ...)`，自动带上拦截器注入的 `trace_id` 等链路字段。
- 内嵌 `UnimplementedXxxServiceServer` 保证新增 RPC 时旧实现仍可编译（前向兼容）。
- **返回对象一律是副本**：`GetUser` / `ListUsers` / `CreateUser` / `UpdateUser` 都通过 `cloneUser` (`proto.Clone`) 返回深拷贝，调用方拿到的指针与内部 map 解耦。`UpdateUser` 同时采用 **COW**（克隆现有对象 → 改字段 → 整体替换回 map），避免与并发的 Get/List 形成读写竞争。
- **分页防御**：`ListUsers` 在 int64 中计算偏移并 clamp，单页大小被 `maxListPageSize = 1000` 硬限制；map 遍历顺序不稳定，统一按 `id` 排序后切片，保证跨页结果可重复。

## interceptor/ — 拦截器与中间件

| 文件                 | 作用域             | 导出                                                                                                                      | 说明                                                                                                                                                                                                                                                     |
| -------------------- | ------------------ | ------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `recovery.go`        | gRPC               | `UnaryRecoveryInterceptor`                                                                                                | 捕获 handler panic → 打印堆栈到服务端日志 → 返回固定 `codes.Internal` 错误（**不把 panic 内容回写给客户端**，避免泄露内部状态）。基于 go-grpc-middleware recovery                                                                                        |
| `trace.go`           | gRPC + HTTP        | `UnaryTraceContextInterceptor`、`TraceResponseHeaderMiddleware`、`TraceAwareErrorHandler`、`GatewayOutgoingHeaderMatcher` | 把 OTel SpanContext 桥接成 `pkg/log` 链路字段，并把 `trace_id` / `span_id` 回传给请求方（成功 / 失败都带）：gRPC 走响应 header metadata（`x-trace-id` / `x-span-id`）；HTTP 走 `X-Trace-Id` / `X-Span-Id` 响应头 + 错误响应体注入 `trace_id` / `span_id` |
| `logging.go`         | gRPC               | `UnaryLoggingInterceptor`                                                                                                 | 请求结束打一行汇总日志（方法 / 耗时 / 状态），底层走 `pkg/log` 并合并 context 链路字段（基于 go-grpc-middleware logging）                                                                                                                                |
| `auth.go`            | gRPC（+HTTP 透传） | `UnaryAuthInterceptor`                                                                                                    | 从 `authorization` metadata 取 Bearer token → `pkg/jwt.Manager` 校验 → Claims 注入 context；`selector` 实现免鉴权白名单                                                                                                                                  |
| `http_middleware.go` | HTTP               | `CORSMiddleware`、`CORSMiddlewareWith`、`CORSOptions`、`HTTPLoggingMiddleware`                                            | CORS 走 rs/cors，策略由 `config.ServerConfig.CORS` 驱动（白名单 / Methods / Headers / Credentials 均可配）；访问日志走 gorilla/handlers Apache 格式                                                                                                      |

### 拦截器链顺序（在 `cmd/server/main.go` 装配）

gRPC Unary 链（外→内）：

```
recovery → trace → logging → auth → 业务 handler
```

- `recovery` 最外层兜底所有 panic。
- `trace` **必须**在 `logging` 之前：先把 `trace_id` 注入 context，访问日志才能带上它。
- `auth` 在 `logging` 之后：未鉴权请求也会被记录。

HTTP 中间件链（外→内）：

```
otelhttp → TraceResponseHeader → CORS → HTTPLogging → gateway mux
```

### 鉴权 HTTP/gRPC 共用

HTTP 请求经 grpc-gateway 转发时，`Authorization` header 会自动透传成 gRPC metadata，因此 `auth.go` 这**一份** JWT 校验逻辑同时覆盖两个入口，无需为 HTTP 单独写鉴权。

## admin/ — 运维/可观测性服务

| 文件       | 导出                                                   | 说明                                                                                                                                                   |
| ---------- | ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `admin.go` | `NewServer(Options)`、`NewHandler(Options)`、`Options` | 独立端口（默认 `:9090`）暴露 `/metrics`（Prometheus）、`/healthz`（存活）、`/readyz`（就绪，`Options.ReadinessCheck` 回调返回非 nil 即 503）、`/debug/pprof`（按配置开关）。`cmd/server/main.go` 把 DB / Redis 的 PingContext 汇总成探针，依赖恢复自动复就绪 |

要点：

- **端口隔离**：与业务 gRPC/HTTP 分开，仅在内网/受信网络暴露，避免泄露指标与剖析信息。
- **指标来源**：`MetricsHandler` 由 `pkg/otel.InitMetrics` 提供；gRPC 端另有标准 `grpc.health.v1.Health` 服务在 `cmd/server/main.go` 注册（供 gRPC 探针）。
- 配置项见 [`config/AGENTS.md`](../config/AGENTS.md) 的 `observability` 段。

## 常见入口

| 想做什么           | 改哪里                                                                                          |
| ------------------ | ----------------------------------------------------------------------------------------------- |
| 改某接口业务逻辑   | `service/user.go` 对应方法                                                                      |
| 实现新 RPC         | 先改 `proto`（见 [`proto/AGENTS.md`](../proto/AGENTS.md)）→ `make proto` → 在 `service/` 加方法 |
| 加免鉴权方法       | `auth.go` 的 `authWhitelist` 加 FullMethod                                                      |
| 加一个 gRPC 拦截器 | `interceptor/` 新建文件 → 在 `cmd/server/main.go` 的 `ChainUnaryInterceptor` 按顺序插入         |
| 加一个 HTTP 中间件 | `http_middleware.go` 加函数 → 在 `cmd/server/main.go` 的 handler 包裹链插入（注意外→内顺序）    |
| 换真实存储         | 给 `UserServer` 加 DB/Redis 依赖字段 + 构造函数注入，替换内存 map；用 `db.Get().WithContext(ctx)` 保留 trace |
