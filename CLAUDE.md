# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository docs system

This project uses a **layered `AGENTS.md` cascade** as its primary documentation. Read it before working on the code — facts live in the layer closest to the source, and upper layers only summarize and link down. Do not duplicate facts across layers.

- [`AGENTS.md`](AGENTS.md) — L0: architecture overview, top-level directory map, "where to change X" table.
- [`proto/AGENTS.md`](proto/AGENTS.md) — Protobuf protocol + HTTP↔gRPC routing (`google.api.http`).
- [`internal/AGENTS.md`](internal/AGENTS.md) — `service/` handlers, `interceptor/` chain, `admin/` ops endpoints.
- [`pkg/AGENTS.md`](pkg/AGENTS.md) — Reusable components: `jwt/`, `log/`, `otel/`.
- [`config/AGENTS.md`](config/AGENTS.md) — Config loading, multi-env, hot-reload.

When adding a fact, put it in the lowest applicable `AGENTS.md`. L0 ≤ 3KB, L2 ≤ 8KB — if you blow the budget the module should be split.

## Common commands

```bash
make install-tools   # one-time: install protoc-gen-go / -go-grpc / -grpc-gateway plugins
make proto           # regenerate gen/proto/*.pb.go after editing proto/user.proto
make dev             # go run ./cmd/server (gRPC :5568, HTTP :8080, admin :9090)
make build           # full pipeline: clean + tidy + fumpt + lint + binary to bin/
make compile         # quick build, skips lint
make debug           # build with -gcflags="all=-N -l" for delve
make test            # go test -v ./...
make fumpt           # gofumpt -w
make lint            # golangci-lint run ./...
```

Run a single test: `go test -v -run TestName ./path/to/pkg`.

`make tidy` uses `go mod tidy -e` on purpose — testify pulls in `go-spew`/`go-difflib` (no `go.mod`, only consumed by other packages' tests) and the module graph reports them as "does not contain package"; the `-e` tolerates this. Don't "fix" it by switching to plain `go mod tidy`.

## Architecture in one paragraph

Single Go process exposes both gRPC (`:5568`) and HTTP/REST (`:8080`) for the **same** `UserService` business logic. HTTP→gRPC translation is handled by [grpc-gateway](https://github.com/grpc-ecosystem/grpc-gateway): `proto/user.proto` declares `google.api.http` annotations, `make proto` regenerates `gen/proto/*.pb.gw.go`, and the gateway proxies HTTP/JSON into a loopback gRPC call. Business handlers in `internal/service` are written **once** and serve both entry points. A third port (`:9090`, `internal/admin`) carries `/metrics`, `/healthz`, `/readyz`, `/debug/pprof` — operational endpoints kept off the business ports.

**Critical invariants** (also stated in [AGENTS.md](AGENTS.md)):

- **Auth is written once.** HTTP `Authorization` header is auto-forwarded by grpc-gateway into gRPC metadata, so `UnaryAuthInterceptor` (JWT) covers both entry points. Don't add a parallel HTTP auth layer.
- **gRPC Unary interceptor chain order (outer→inner):** `recovery → trace → logging → auth → handler`. `trace` must come **before** `logging` so the access log carries `trace_id`. Configured in `cmd/server/main.go`.
- **HTTP middleware chain (outer→inner):** `otelhttp → TraceResponseHeader → CORS → HTTPLogging → gateway mux`. CORS策略来自 `config.Server.CORS`，`allow_credentials=true && origins=["*"]` 在启动期 fail-fast 拒绝。
- **Recovery never echoes panic content to the client.** `interceptor.UnaryRecoveryInterceptor` 把堆栈与值仅写日志，对外只返回固定 `codes.Internal "internal server error"`，避免泄露内部状态（SQL、连接串等）。
- **Service handlers return clones.** `internal/service/user.go` 通过 `cloneUser` (`proto.Clone`) 返回深拷贝；`UpdateUser` 用 COW（克隆 → 改字段 → 替换回 map），避免与并发 Get/List 形成读写竞争。`ListUsers` 按 id 稳定排序后切片，分页结果可重复；偏移量在 int64 中计算并 clamp，单页 size 被 `maxListPageSize=1000` 限制。
- **Trace continuity.** HTTP entry and loopback gRPC end up in the same trace via W3C `traceparent` (otelhttp + otelgrpc). `trace_id` is injected into logs by `interceptor/trace.go` and read by `log.InfoContext`.
- **`InitMetrics` must run before** any otelgrpc/otelhttp instrumentation is created — it sets the global `MeterProvider`, after which RED metrics flow with zero business-side instrumentation.
- **`config.Init` is idempotent and `Stop`-safe.** 进程内多次调用只首次生效；`Stop` 之后即便仍有 fsnotify 定时器派发，`doReload` 也会被 `watchClosed` 守卫短路，不再修改全局配置指针。
- **DB / Redis 按需启用 + 自动接 OTel.** `config.{Database,Redis}.Enabled=true` 时 `main` 调 `pkg/db`、`pkg/redis` 的 `Init` 建连接池；`tracing.enabled=true` 时再调 `InstrumentTracing()` 挂 otelgorm / redisotel hook —— 业务侧务必用 `db.Get().WithContext(ctx)` 才能让 SQL span 续在请求 trace 上。两者同时也是 `/readyz` 的就绪依赖（2s PingContext，超时即 503，恢复自动复就绪）。
- **限流器 Lua 由 NewScript 缓存 + 单调 member.** `pkg/redis.SlidingWindowLimiter` 用 `redis.NewScript` 自动 EVALSHA；ZSet member 由进程内 `atomic.Uint64` 单调序列拼成，根本上避免「同毫秒并发被 ZADD 去重吞计数」的偏紧问题。

## Config model

- File: `config/app.{APP_ENV}.yaml`, fallback `config/app.yaml`. `APP_ENV` default `dev`.
- `${VAR}` in yaml is expanded by `os.ExpandEnv` at load time.
- Hot reload via `fsnotify` (watching the **directory**, not the file — editors rename on save). Read end is lock-free `atomic.Pointer[Config]`; **callers must not mutate** the returned `*Config`.
- Field semantics in [`config/types.go`](config/types.go) comments distinguish **【需重启】** (cached into servers/pools at startup) from **【热更生效】** (read on every request). When you add a field, mark it.
- Components register hot-reload reactions via `config.OnReload(func(){ ... })`. Example: `cmd/server/main.go` registers a log-level updater.

## Where to make changes (cheat sheet)

| Want to...                            | Go to                                                                                                                         |
| ------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Add/change an RPC or wire format      | `proto/user.proto` → `make proto` → implement in `internal/service`                                                           |
| Change business logic for an endpoint | `internal/service/user.go`                                                                                                    |
| Add a gRPC interceptor                | New file in `internal/interceptor/` → insert into `ChainUnaryInterceptor` in `cmd/server/main.go` (mind the order rule above) |
| Add an HTTP middleware                | `internal/interceptor/http_middleware.go` → wrap into the handler chain in `cmd/server/main.go`                               |
| Tune CORS policy                      | `config.server.cors` 段（白名单 / 方法 / Headers / Credentials）                                                              |
| Allow-list a method for auth          | `authWhitelist` in `internal/interceptor/auth.go` (FullMethod string)                                                         |
| Add a config field                    | `config/types.go` (+ default in `newDefaultConfig`) + `config/app.yaml.example`                                               |
| Add a custom business metric          | Use the global `MeterProvider` (`otel.GetMeterProvider().Meter(...)`) — it's plumbed into `/metrics` automatically            |
| Toggle metrics/tracing/pprof          | yaml `observability.{metrics,tracing,pprof}` — no code change                                                                 |

## Local workflow rules

These come from `.codebuddy/rules/` and apply project-wide:

- **No backwards-compat code.** The project is pre-1.0. Don't add fallback branches, don't keep deprecated fields alive, don't pad new code with "for old data" handling. When replacing logic, delete the old version in the same change.
- **Don't proactively create Markdown docs.** No README/guide/explainer files unless explicitly requested. Use code comments or chat for explanations. The `AGENTS.md` cascade is the documentation — extend it, don't shadow it.
- **`gen/proto/` is generated.** Never hand-edit `*.pb.go`, `*_grpc.pb.go`, `*.pb.gw.go`. Change `user.proto` and rerun `make proto`.
- **Proto evolution discipline.** New fields = new tag numbers. Deleting a field → mark `reserved`. Don't change field types — add a new field instead.
