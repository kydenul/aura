// Package admin 装配运维/可观测性 HTTP 服务，统一暴露 /metrics、/healthz、/readyz
// 与 /debug/pprof。这些端点挂在独立端口（与业务 gRPC/HTTP 隔离），仅在内网/受信网络
// 暴露，避免把指标与剖析等敏感信息泄露给外部流量。
package admin

import (
	"net/http"
	"net/http/pprof"
	"time"
)

// Options 控制运维服务暴露的端点。
type Options struct {
	// Addr 监听地址，如 ":9090"。
	Addr string
	// ReadHeaderTimeout 读请求头超时，防 Slowloris 慢速攻击。
	ReadHeaderTimeout time.Duration
	// MetricsPath Prometheus 拉取路径，为空时默认 /metrics。
	MetricsPath string
	// MetricsHandler Prometheus 指标 handler；为 nil 时不暴露 /metrics。
	MetricsHandler http.Handler
	// EnablePprof 是否注册 /debug/pprof 系列端点。
	EnablePprof bool
	// ReadinessCheck 就绪探针回调；返回非 nil 时 /readyz 返回 503。为 nil 时恒就绪。
	ReadinessCheck func() error
}

// NewServer 按 Options 构建运维 HTTP server（未启动）。
func NewServer(opts Options) *http.Server {
	return &http.Server{
		Addr:              opts.Addr,
		Handler:           NewHandler(opts),
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
	}
}

// NewHandler 构建运维端点的路由（独立 mux，不污染 http.DefaultServeMux）。
func NewHandler(opts Options) http.Handler {
	mux := http.NewServeMux()

	// 存活探针：进程能响应即视为存活。
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})

	// 就绪探针：依赖未就绪时返回 503，供负载均衡 / K8s 摘流。
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if opts.ReadinessCheck != nil {
			if err := opts.ReadinessCheck(); err != nil {
				writeText(w, http.StatusServiceUnavailable, "not ready: "+err.Error())
				return
			}
		}
		writeText(w, http.StatusOK, "ok")
	})

	if opts.MetricsHandler != nil {
		path := opts.MetricsPath
		if path == "" {
			path = "/metrics"
		}
		mux.Handle(path, opts.MetricsHandler)
	}

	if opts.EnablePprof {
		registerPprof(mux)
	}

	return mux
}

// registerPprof 显式挂载 net/http/pprof 端点到指定 mux（避免依赖 DefaultServeMux 的隐式注册）。
func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

func writeText(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg))
}
