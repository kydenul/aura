package interceptor

import (
	"net/http"
	"os"

	"github.com/gorilla/handlers"
	"github.com/rs/cors"
)

// CORSOptions 描述 HTTP CORS 中间件的策略配置。
type CORSOptions struct {
	AllowedOrigins   []string
	AllowedMethods   []string
	AllowedHeaders   []string
	AllowCredentials bool
}

// 默认 CORS 策略：开发友好（任意源、常见方法/头），不允许 credentials。
// 生产环境务必通过配置显式收敛 AllowedOrigins / AllowedHeaders。
var defaultCORSOptions = CORSOptions{
	AllowedOrigins: []string{"*"},
	AllowedMethods: []string{
		http.MethodGet, http.MethodPost,
		http.MethodPatch, http.MethodDelete,
		http.MethodOptions,
	},
	AllowedHeaders:   []string{"Content-Type", "Authorization"},
	AllowCredentials: false,
}

// CORSMiddleware 使用社区事实标准库 rs/cors 实现跨域处理：
//   - 自动处理 OPTIONS 预检
//   - 配置项丰富（白名单域名、暴露 header、credentials 等）
//
// 调用方传入 nil 时退化到默认策略（开发友好），不要在生产用默认值。
func CORSMiddleware(next http.Handler) http.Handler {
	return CORSMiddlewareWith(defaultCORSOptions, next)
}

// CORSMiddlewareWith 以指定 CORS 策略构造中间件；空字段回落到默认策略对应项，
// 使「只覆盖 AllowedOrigins」这种部分配置依旧可用。
func CORSMiddlewareWith(opts CORSOptions, next http.Handler) http.Handler {
	if len(opts.AllowedOrigins) == 0 {
		opts.AllowedOrigins = defaultCORSOptions.AllowedOrigins
	}
	if len(opts.AllowedMethods) == 0 {
		opts.AllowedMethods = defaultCORSOptions.AllowedMethods
	}
	if len(opts.AllowedHeaders) == 0 {
		opts.AllowedHeaders = defaultCORSOptions.AllowedHeaders
	}
	c := cors.New(cors.Options{
		AllowedOrigins:   opts.AllowedOrigins,
		AllowedMethods:   opts.AllowedMethods,
		AllowedHeaders:   opts.AllowedHeaders,
		AllowCredentials: opts.AllowCredentials,
	})
	return c.Handler(next)
}

// HTTPLoggingMiddleware 使用 gorilla/handlers 提供的 LoggingHandler，
// 输出 Apache Common Log 格式（method / path / status / size / 耗时），
// 是 Go 社区里最常见的 HTTP 访问日志格式之一，可以直接被 GoAccess 等工具解析。
//
// 自实现已替换为成熟库：github.com/gorilla/handlers
func HTTPLoggingMiddleware(next http.Handler) http.Handler {
	return handlers.CombinedLoggingHandler(os.Stdout, next)
}
