package config

import "time"

// 配置默认值常量（被 newDefaultConfig 使用，单元测试也引用，避免散落的魔法字符串）。
const (
	defaultGRPCAddr   = ":5568"    // gRPC 原生端口默认值
	defaultHTTPAddr   = ":8080"    // HTTP REST 网关端口默认值
	defaultAdminAddr  = ":9090"    // 运维/可观测性端口默认值
	defaultMetricsPth = "/metrics" // Prometheus 拉取路径默认值
)

// Config 是应用的全量配置（YAML 映射）。
//
// 设计约定：
//   - 【需重启】启动时被缓存到连接池 / 单例 / server 字段，改 yaml 后必须重启才能生效。
//   - 【热更生效】业务路径每次通过 config.Get() 实时读取，改 yaml 后立即生效。
//
// 字段按业务域分组为嵌套子结构，便于阅读和扩展。
type Config struct {
	// Server gRPC / HTTP 监听地址及超时（【需重启】，启动时绑定端口）。
	Server ServerConfig `yaml:"server"`

	// JWT token 签发与校验参数（【需重启】，启动时构建 jwt.Manager 单例）。
	JWT JWTConfig `yaml:"jwt"`

	// HMAC 请求签名鉴权（面向 OpenAPI / 机器对机器调用，【需重启】，启动时构建 hmac.Manager）。
	HMAC HMACConfig `yaml:"hmac"`

	// RateLimit gRPC 入口的单机过载保护限流（开关与阈值【热更生效】，经 OnReload 调 Limiter.Update）。
	RateLimit RateLimitConfig `yaml:"ratelimit"`

	// Log 日志级别与格式（【热更生效】）。
	Log LogConfig `yaml:"log"`

	// Database 关系型数据库连接（【需重启】，启动时建立连接池）。
	Database DatabaseConfig `yaml:"database"`

	// Redis 缓存连接（【需重启】，启动时建立连接池）。
	Redis RedisConfig `yaml:"redis"`

	// Observability 可观测性：指标 / 链路上报 / 性能剖析 / 健康检查（【需重启】，启动时装配）。
	Observability ObservabilityConfig `yaml:"observability"`
}

// ObservabilityConfig 可观测性总配置。
//
// metrics / health / pprof 统一挂在独立的运维端口 AdminAddr 上，与业务端口（gRPC/HTTP）隔离，
// 仅在内网 / 受信网络暴露，避免把 /metrics、/debug/pprof 等敏感端点暴露给外部流量。
type ObservabilityConfig struct {
	// AdminAddr 运维/可观测性 HTTP 端口，承载 /metrics、/healthz、/readyz、/debug/pprof。
	AdminAddr string `yaml:"admin_addr"`

	// Metrics Prometheus 指标采集。
	Metrics MetricsConfig `yaml:"metrics"`
	// Tracing OpenTelemetry 链路追踪与上报。
	Tracing TracingConfig `yaml:"tracing"`
	// Pprof Go 运行时性能剖析端点（默认关闭，仅排障时开启）。
	Pprof PprofConfig `yaml:"pprof"`
}

// MetricsConfig 指标采集配置。
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"` // 是否暴露 Prometheus /metrics 端点
	Path    string `yaml:"path"`    // 拉取路径，默认 /metrics
}

// TracingConfig 链路追踪配置。
//
// 即使 Exporter 为 "none"，只要 Enabled=true 仍会装配 TracerProvider 与 W3C 传播器，
// 保证每条请求生成有效 SpanContext（日志稳定带 trace_id）；"none" 只是不向后端上报 span。
type TracingConfig struct {
	Enabled bool `yaml:"enabled"` // 是否启用链路追踪（关闭后日志将不再带 trace_id）

	// Exporter span 上报方式：none（仅进程内 / 不上报）| otlp（上报到 OTLP collector）。
	Exporter string `yaml:"exporter"`
	// Endpoint OTLP collector 地址（exporter=otlp 时必填），如 "localhost:4317"。
	Endpoint string `yaml:"endpoint"`
	// Insecure OTLP 连接是否禁用 TLS（明文 gRPC），内网 collector 常用 true。
	Insecure bool `yaml:"insecure"`
	// SampleRatio 采样比例 [0,1]：>=1 全采样，<=0 不采样（仍生成 trace_id），其余为按 TraceID 比例采样。
	SampleRatio float64 `yaml:"sample_ratio"`
}

// PprofConfig 性能剖析配置。
type PprofConfig struct {
	Enabled bool `yaml:"enabled"` // 是否暴露 /debug/pprof（含 CPU / heap / goroutine 等）
}

// ServerConfig 服务监听相关配置。
type ServerConfig struct {
	GRPCAddr string `yaml:"grpc_addr"` // gRPC 原生端口，如 ":5568"
	HTTPAddr string `yaml:"http_addr"` // HTTP REST 网关端口，如 ":8080"

	// ReadHeaderTimeoutSecs 读取请求头超时（秒），防止 Slowloris 慢速攻击。
	ReadHeaderTimeoutSecs int `yaml:"read_header_timeout_secs"`
	// ShutdownTimeoutSecs 优雅关闭等待超时（秒）。
	ShutdownTimeoutSecs int `yaml:"shutdown_timeout_secs"`

	// CORS HTTP 跨域策略（仅影响 HTTPAddr）。
	CORS CORSConfig `yaml:"cors"`
}

// CORSConfig HTTP CORS 中间件配置。
//
// 配置项语义对齐 github.com/rs/cors：
//   - AllowedOrigins 为空时默认 ["*"]（开发友好），生产应显式收敛白名单。
//   - AllowCredentials=true 与 AllowedOrigins=["*"] 同时存在不符合 CORS 规范，
//     启动时会被 cmd/server 拒绝。
type CORSConfig struct {
	AllowedOrigins   []string `yaml:"allowed_origins"`
	AllowedMethods   []string `yaml:"allowed_methods"`
	AllowedHeaders   []string `yaml:"allowed_headers"`
	AllowCredentials bool     `yaml:"allow_credentials"`
}

// JWTConfig token 签发与校验配置。
type JWTConfig struct {
	// Secret HMAC 签名密钥，生产环境建议用 ${JWT_SECRET} 从环境变量注入，切勿硬编码。
	Secret string `yaml:"secret"`
	// Issuer 签发者标识，写入 iss 并在校验时强制比对。
	Issuer string `yaml:"issuer"`
	// TTLHours token 有效期（小时），<=0 时使用代码默认值。
	TTLHours int `yaml:"ttl_hours"`
}

// TTL 返回 token 有效期 time.Duration。
func (c JWTConfig) TTL() time.Duration {
	return time.Duration(c.TTLHours) * time.Hour
}

// HMACConfig 请求签名鉴权配置（TC-HMAC-SHA256），面向服务间 / OpenAPI 可信调用。
// 算法见《JWT + HMAC 双层鉴权方案》外层 HMAC 规范，实现见 pkg/hmac。
//
// 默认 enabled=false，此时 HTTP 网关不挂签名中间件，对现有 JWT 流程零影响；
// 开启后命中 ProtectedPrefixes 的 HTTP 请求需携带 X-Auth-AppId / X-Auth-Timestamp /
// X-Auth-Nonce 以及 X-Auth-Sign: TC-HMAC-SHA256 Credential=<appid>,Signature=<SIG>。
// 签名走独立头，不占用 Authorization（Authorization: Bearer 留给 JWT）。
type HMACConfig struct {
	// Enabled 是否启用 HMAC 请求签名鉴权（关闭则启动期不构建 Manager、HTTP 网关不挂中间件）。
	Enabled bool `yaml:"enabled"`
	// SkewSecs 允许的请求时间戳与服务端时钟最大偏移（秒），用于防重放；<=0 时使用代码默认值 300。
	SkewSecs int `yaml:"skew_secs"`
	// ProtectedPrefixes 仅对路径命中任一前缀的请求验签；为空表示保护全部路由。
	// 用于把"机器调用走 HMAC 的开放接口"与"用户走 JWT 的普通接口"分开，避免全员双重鉴权。
	ProtectedPrefixes []string `yaml:"protected_prefixes"`
	// Keys 调用方凭据列表（app_id → secret），enabled=true 时至少需配置一条。
	Keys []HMACKeyPair `yaml:"keys"`
}

// HMACKeyPair 一条 HMAC 调用方凭据：app_id 即 X-Auth-AppId / X-Auth-Sign 中的 Credential，
// secret 为共享密钥（建议通过环境变量注入，避免硬编码到 yaml）。
type HMACKeyPair struct {
	AppID  string `yaml:"app_id"`
	Secret string `yaml:"secret"`
}

// Skew 返回允许的时间窗 time.Duration。
func (c HMACConfig) Skew() time.Duration {
	return time.Duration(c.SkewSecs) * time.Second
}

// KeyMap 把 Keys 列表投影成 app_id → secret 的 map，供 hmac.NewManager 使用。
func (c HMACConfig) KeyMap() map[string]string {
	m := make(map[string]string, len(c.Keys))
	for _, kp := range c.Keys {
		if kp.AppID == "" || kp.Secret == "" {
			continue
		}
		m[kp.AppID] = kp.Secret
	}
	return m
}

// LogConfig 日志配置。
//
// Level / Format【热更生效】；Output 与 File.* 涉及输出目标与文件句柄，
// 启动时装配，属【需重启】。
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json

	// Output 输出目标：stdout（默认，容器/云原生推荐）| file | both。
	Output string `yaml:"output"`
	// File 文件滚动参数，仅当 Output 为 file / both 时生效。
	File LogFileConfig `yaml:"file"`
}

// LogFileConfig 日志文件滚动（rotation）参数，底层由 lumberjack 实现，
// 防止日志文件无限膨胀导致磁盘占满 / 检索困难。
type LogFileConfig struct {
	Path       string `yaml:"path"`         // 日志文件路径，Output 含 file 时必填
	MaxSizeMB  int    `yaml:"max_size_mb"`  // 单文件最大体积（MB），超过即切割
	MaxBackups int    `yaml:"max_backups"`  // 最多保留的旧文件个数
	MaxAgeDays int    `yaml:"max_age_days"` // 旧文件最长保留天数
	Compress   bool   `yaml:"compress"`     // 是否 gzip 压缩切割后的旧文件
}

// RateLimitConfig 单机令牌桶过载保护配置（实现见 pkg/ratelimit）。
//
// 挂在 gRPC 入口拦截器上：由于 HTTP REST 经 grpc-gateway 同进程 loopback 转发到 gRPC，
// 这一道限流可同时覆盖 gRPC 直连与 HTTP 两个入口（与 JWT 鉴权同款契约），HTTP 中间件无需重复挂。
// 被限流时 gRPC 返回 codes.ResourceExhausted，网关自动映射为 HTTP 429。
//
// 这是「单实例」过载保护（进程内、零外部依赖）：多副本各自独立计数，整体容量 ≈ rps × 副本数。
// enabled / rps / burst 均【热更生效】：改 yaml 后经 OnReload 调 Limiter.Update 即时切换，无需重启。
type RateLimitConfig struct {
	// Enabled 是否启用过载保护；false 或 rps<=0 时恒放行（不限流）。
	Enabled bool `yaml:"enabled"`
	// RPS 整机每秒允许的平均请求数（令牌产生速率）。
	RPS float64 `yaml:"rps"`
	// Burst 突发桶容量（瞬时可同时通过的最大请求数）；<1 时自动取 1。
	Burst int `yaml:"burst"`
}

// DatabaseConfig 关系型数据库配置。
type DatabaseConfig struct {
	Enabled             bool   `yaml:"enabled"`                // 是否启用数据库（关闭则启动期不建连接池）
	Driver              string `yaml:"driver"`                 // 数据库驱动：mysql | postgres，留空按 mysql 处理
	DSN                 string `yaml:"dsn"`                    // 数据源名称，如 mysql/postgres 连接串
	MaxOpenConns        int    `yaml:"max_open_conns"`         // 最大打开连接数
	MaxIdleConns        int    `yaml:"max_idle_conns"`         // 最大空闲连接数
	ConnMaxLifetimeSecs int    `yaml:"conn_max_lifetime_secs"` // 连接最大存活时间（秒）
}

// RedisConfig 缓存连接配置。
type RedisConfig struct {
	Enabled          bool   `yaml:"enabled"` // 是否启用 Redis（关闭则启动期不建连接池）
	Host             string `yaml:"host"`
	Port             int    `yaml:"port"`
	Password         string `yaml:"password"`
	DB               int    `yaml:"db"`
	PoolSize         int    `yaml:"pool_size"`
	MinIdleConns     int    `yaml:"min_idle_conns"`
	DialTimeoutSecs  int    `yaml:"dial_timeout_secs"`
	ReadTimeoutSecs  int    `yaml:"read_timeout_secs"`
	WriteTimeoutSecs int    `yaml:"write_timeout_secs"`
	MaxRetries       int    `yaml:"max_retries"`
}

// newDefaultConfig 返回带全部默认值的 Config。
// yaml.Unmarshal 只覆盖 yaml 文件中实际出现的字段，未出现的字段保留这里的默认值。
func newDefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			GRPCAddr:              defaultGRPCAddr,
			HTTPAddr:              defaultHTTPAddr,
			ReadHeaderTimeoutSecs: 10,
			ShutdownTimeoutSecs:   10,
			CORS: CORSConfig{
				AllowedOrigins: []string{"*"},
				AllowedMethods: []string{
					"GET", "POST", "PATCH", "DELETE", "OPTIONS",
				},
				AllowedHeaders:   []string{"Content-Type", "Authorization"},
				AllowCredentials: false,
			},
		},
		JWT: JWTConfig{
			Issuer:   "aura",
			TTLHours: 2,
		},
		HMAC: HMACConfig{
			Enabled:  false,
			SkewSecs: 300,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
			Output: "stdout",
			File: LogFileConfig{
				Path:       "logs/aura.log",
				MaxSizeMB:  100,
				MaxBackups: 10,
				MaxAgeDays: 30,
				Compress:   true,
			},
		},
		RateLimit: RateLimitConfig{
			Enabled: false,
			RPS:     0,
			Burst:   0,
		},
		Database: DatabaseConfig{
			Driver:              "mysql",
			MaxOpenConns:        50,
			MaxIdleConns:        10,
			ConnMaxLifetimeSecs: 1800,
		},
		Redis: RedisConfig{
			Host:             "127.0.0.1",
			Port:             6379,
			DB:               0,
			PoolSize:         10,
			MinIdleConns:     2,
			DialTimeoutSecs:  5,
			ReadTimeoutSecs:  3,
			WriteTimeoutSecs: 3,
			MaxRetries:       2,
		},
		Observability: ObservabilityConfig{
			AdminAddr: defaultAdminAddr,
			Metrics: MetricsConfig{
				Enabled: true,
				Path:    defaultMetricsPth,
			},
			Tracing: TracingConfig{
				Enabled:     true,
				Exporter:    "none",
				Insecure:    true,
				SampleRatio: 1.0,
			},
			Pprof: PprofConfig{
				Enabled: false,
			},
		},
	}
}
