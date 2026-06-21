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

// LogConfig 日志配置。
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
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
		Log: LogConfig{
			Level:  "info",
			Format: "text",
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
