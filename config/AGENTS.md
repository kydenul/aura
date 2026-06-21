# config · L2

应用配置：加载、多环境解析、热更新。完全基于成熟开源库（`yaml.v3` 解析 + `fsnotify` 监听），不依赖任何框架私有组件，也**不反向 import 业务/组件包**（避免循环依赖）。

## 文件

| 文件 | 职责 |
|------|------|
| `config.go` | 加载入口 `Init` / 读端 `Get` / 多环境路径解析 / `${ENV_VAR}` 展开 / `OnReload` 回调注册 / diff 日志 |
| `types.go` | `Config` 全量结构体（按业务域分组的嵌套子结构）+ `newDefaultConfig` 默认值 |
| `watcher.go` | `fsnotify` 目录监听 + 防抖（200ms）触发热更 |
| `config_test.go` | 单元测试 |
| `app.yaml.example` | 配置模板（复制为 `app.yaml` 后填值） |

## 核心机制

- **多环境**：`APP_ENV` 选择 `config/app.{env}.yaml`，不存在回退 `config/app.yaml`；`APP_ENV` 默认 `dev`。`Env()` / `IsDev()` 是全局唯一读 `APP_ENV` 的地方。
- **环境变量展开**：解析前 `os.ExpandEnv` 展开 yaml 里的 `${VAR}`，适配 CI/CD 与容器。
- **无锁并发安全读**：`atomic.Pointer[Config]` 保存快照，`Get()` 读端无锁；调用方**不应修改**返回内容。
- **Init 幂等**：`Init` 用 `sync.Once` 守护，进程内多次调用只首次生效（返回相同 error）；测试中可通过包内私有的 `resetForTesting` 还原所有状态。
- **热更**：监听配置文件**所在目录**（而非文件本身——很多编辑器用「写临时文件 + 原子 rename」保存，直接监听文件会在 rename 后丢 inode）；相关事件经防抖后 `doReload` 重新加载并原子替换；解析失败保留旧配置不中断服务。
- **关停安全**：`stopWatch` 通过 `watchClosed` 标记同时守住「事件回调入口」与「`doReload` 入口」，已派发但未拿锁的定时器回调不会再修改全局配置。
- **diff 日志**：热更后对比新旧 yaml 顶层 key，打印新增 / 变更 / 移除项。
- **`OnReload` 回调**：组件在启动期注册，热更成功后按注册顺序串行执行（单个 panic 不影响其他回调）；用于「根据新配置重建内部状态」。本包不主动调业务包，由各组件挂载回调（如 `cmd/server/main.go` 注册了「热更日志级别」）。

## 配置语义约定（编辑 types.go 必读）

`Config` 字段分两类，注释里已用标记区分：

- **【需重启】**：启动时被缓存到 server / 单例 / 连接池（`Server` 端口、`JWT`、`Database`、`Redis`、`Observability`），改 yaml 后须重启才生效。
- **【热更生效】**：业务路径每次通过 `config.Get()` 实时读取（如 `Log`），改 yaml 立即生效。

> 具体字段直接读 `types.go`（每个字段都有行内注释），不在此重复。

### observability 段速览

| 子段 | 关键字段 | 作用 |
|------|---------|------|
| `metrics` | `enabled` / `path` | 是否暴露 Prometheus `/metrics` 及路径 |
| `tracing` | `enabled` / `exporter`(none\|otlp) / `endpoint` / `insecure` / `sample_ratio` | 链路追踪与 span 上报；`enabled=false` 后日志不再带 `trace_id` |
| `pprof` | `enabled` | `/debug/pprof` 开关（默认关，排障时临时开） |
| 顶层 | `admin_addr` | 运维端口，统一承载上述端点（与业务端口隔离） |

### server.cors 段速览

HTTP CORS 策略集中在 `server.cors`，给生产显式收敛白名单的位置：

| 字段 | 作用 |
|------|------|
| `allowed_origins` | 允许的 Origin 列表，缺省 `["*"]`（仅适合本地开发） |
| `allowed_methods` | 允许的 HTTP 方法 |
| `allowed_headers` | 允许的请求头 |
| `allow_credentials` | 是否允许带凭证；`true` 时与 `allowed_origins=["*"]` 同时存在违反 CORS 规范，启动期 fail-fast |

## 常见入口

| 想做什么 | 改哪里 |
|---------|-------|
| 加配置项 | `types.go` 对应子结构加字段（带 `yaml` tag）+ `newDefaultConfig` 给默认值 + 更新 `app.yaml.example` |
| 加一个配置子域 | `types.go` 新建 `XxxConfig` 结构体 + 挂到 `Config` + 默认值 |
| 让组件响应热更 | 在组件初始化处 `config.OnReload(func(){ ... config.Get() ... })` 注册回调 |
| 测试里覆盖配置 | `config.SetForTesting(cfg)` 或 `Init(config.WithPath(...), config.WithWatch(false))` |
