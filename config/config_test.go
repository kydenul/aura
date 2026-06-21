package config

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"aura/pkg/log"
)

// ── 测试辅助 ──────────────────────────────────────────────────────────────────

// writeFile 写入（或覆盖）一个文件，失败即终止测试。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入文件 %q 失败: %v", path, err)
	}
}

// cleanupGlobals 注册清理：停止监听并重置所有包级全局状态，避免测试间互相污染。
func cleanupGlobals(t *testing.T) {
	t.Helper()
	resetForTesting()
	t.Cleanup(func() {
		resetForTesting()
		reloadCallbacksMu.Lock()
		reloadCallbacks = nil
		reloadCallbacksMu.Unlock()
	})
}

// captureLog 把统一日志组件的输出重定向到 buffer，返回读取函数；测试结束自动还原。
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	restore := log.SetOutputForTesting(&buf)
	t.Cleanup(restore)
	return buf.String
}

// ── 默认值 / 类型 ─────────────────────────────────────────────────────────────

func TestNewDefaultConfig(t *testing.T) {
	cfg := newDefaultConfig()
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"server.grpc_addr", cfg.Server.GRPCAddr, defaultGRPCAddr},
		{"server.http_addr", cfg.Server.HTTPAddr, ":8080"},
		{"server.read_header_timeout_secs", cfg.Server.ReadHeaderTimeoutSecs, 10},
		{"server.shutdown_timeout_secs", cfg.Server.ShutdownTimeoutSecs, 10},
		{"jwt.issuer", cfg.JWT.Issuer, "aura"},
		{"jwt.ttl_hours", cfg.JWT.TTLHours, 2},
		{"log.level", cfg.Log.Level, "info"},
		{"redis.host", cfg.Redis.Host, "127.0.0.1"},
		{"redis.port", cfg.Redis.Port, 6379},
		{"redis.max_retries", cfg.Redis.MaxRetries, 2},
		{"database.max_open_conns", cfg.Database.MaxOpenConns, 50},
		{"observability.admin_addr", cfg.Observability.AdminAddr, defaultAdminAddr},
		{"observability.metrics.enabled", cfg.Observability.Metrics.Enabled, true},
		{"observability.metrics.path", cfg.Observability.Metrics.Path, defaultMetricsPth},
		{"observability.tracing.enabled", cfg.Observability.Tracing.Enabled, true},
		{"observability.tracing.exporter", cfg.Observability.Tracing.Exporter, "none"},
		{"observability.tracing.sample_ratio", cfg.Observability.Tracing.SampleRatio, 1.0},
		{"observability.pprof.enabled", cfg.Observability.Pprof.Enabled, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestJWTConfigTTL(t *testing.T) {
	if got := (JWTConfig{TTLHours: 3}).TTL(); got != 3*time.Hour {
		t.Fatalf("TTL() = %v, want %v", got, 3*time.Hour)
	}
	if got := (JWTConfig{TTLHours: 0}).TTL(); got != 0 {
		t.Fatalf("TTL() = %v, want 0", got)
	}
}

// ── loadConfig ────────────────────────────────────────────────────────────────

func TestLoadConfigKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	// 只覆盖 http_addr，其余字段应保留默认值。
	writeFile(t, path, "server:\n  http_addr: \":18080\"\n")

	cfg, raw, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("raw 字节为空")
	}
	if cfg.Server.HTTPAddr != ":18080" {
		t.Errorf("http_addr = %q, want :18080", cfg.Server.HTTPAddr)
	}
	// 未在 yaml 出现的字段保留默认值。
	if cfg.Server.GRPCAddr != defaultGRPCAddr {
		t.Errorf("grpc_addr = %q, want 默认 %s", cfg.Server.GRPCAddr, defaultGRPCAddr)
	}
	if cfg.JWT.Issuer != "aura" || cfg.JWT.TTLHours != 2 {
		t.Errorf("jwt 默认值未保留: %+v", cfg.JWT)
	}
	if cfg.Redis.Port != 6379 {
		t.Errorf("redis.port = %d, want 默认 6379", cfg.Redis.Port)
	}
}

func TestLoadConfigExpandsEnv(t *testing.T) {
	t.Setenv("TEST_JWT_SECRET", "s3cr3t-value")
	t.Setenv("TEST_REDIS_PORT", "6400")

	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path,
		"jwt:\n  secret: \"${TEST_JWT_SECRET}\"\nredis:\n  port: ${TEST_REDIS_PORT}\n")

	cfg, _, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if cfg.JWT.Secret != "s3cr3t-value" {
		t.Errorf("secret = %q, want 展开后的环境变量值", cfg.JWT.Secret)
	}
	if cfg.Redis.Port != 6400 {
		t.Errorf("redis.port = %d, want 6400", cfg.Redis.Port)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, _, err := loadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("缺失文件应返回错误")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "server: [unclosed\n  bad: : :\n")

	if _, _, err := loadConfig(path); err == nil {
		t.Fatal("非法 yaml 应返回错误")
	}
}

// ── Init / Get ────────────────────────────────────────────────────────────────

func TestInitLoadsConfig(t *testing.T) {
	cleanupGlobals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "server:\n  http_addr: \":12345\"\njwt:\n  issuer: \"unit-test\"\n")

	if err := Init(WithPath(path), WithWatch(false)); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	cfg := Get()
	if cfg == nil {
		t.Fatal("Get() 返回 nil")
	}
	if cfg.Server.HTTPAddr != ":12345" || cfg.JWT.Issuer != "unit-test" {
		t.Fatalf("配置未正确加载: %+v", cfg)
	}
}

func TestInitMissingFile(t *testing.T) {
	cleanupGlobals(t)
	err := Init(WithPath(filepath.Join(t.TempDir(), "missing.yaml")), WithWatch(false))
	if err == nil {
		t.Fatal("配置文件缺失时 Init 应返回错误")
	}
}

func TestInitWatchToggle(t *testing.T) {
	cleanupGlobals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "log:\n  level: \"warn\"\n")

	// 关闭监听：不应创建 watcher。
	if err := Init(WithPath(path), WithWatch(false)); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if watcher != nil {
		t.Fatal("WithWatch(false) 不应创建 watcher")
	}

	// 重置后再以开启监听方式 Init，应创建 watcher（Init 自身是幂等的）。
	resetForTesting()
	if err := Init(WithPath(path), WithWatch(true)); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if watcher == nil {
		t.Fatal("WithWatch(true) 应创建 watcher")
	}
}

func TestInitIsIdempotent(t *testing.T) {
	cleanupGlobals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "server:\n  http_addr: \":11111\"\n")

	if err := Init(WithPath(path), WithWatch(false)); err != nil {
		t.Fatalf("第一次 Init error: %v", err)
	}
	// 第二次 Init 传一份「不同」的配置，应被忽略（initOnce 守卫），仍保留首次的值。
	other := filepath.Join(dir, "app2.yaml")
	writeFile(t, other, "server:\n  http_addr: \":22222\"\n")
	if err := Init(WithPath(other), WithWatch(false)); err != nil {
		t.Fatalf("第二次 Init error: %v", err)
	}
	if got := Get().Server.HTTPAddr; got != ":11111" {
		t.Fatalf("Init 应幂等, 当前 http_addr = %q (期望 :11111)", got)
	}
}

func TestSetForTesting(t *testing.T) {
	cleanupGlobals(t)

	override := &Config{}
	override.Server.HTTPAddr = ":9999"
	SetForTesting(override)
	if Get().Server.HTTPAddr != ":9999" {
		t.Fatal("SetForTesting 未生效")
	}
	// nil 不应覆盖现有配置。
	SetForTesting(nil)
	if Get().Server.HTTPAddr != ":9999" {
		t.Fatal("SetForTesting(nil) 不应覆盖配置")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	cleanupGlobals(t)
	// 未启动监听时调用 Stop 应安全；多次调用也应安全。
	Stop()
	Stop()
}

// 回归：Stop 之后被定时器派发的 reload 不应再修改全局配置。
func TestStopRacesWithPendingReload(t *testing.T) {
	cleanupGlobals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "server:\n  http_addr: \":4444\"\n")

	if err := Init(WithPath(path), WithWatch(false)); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	want := Get().Server.HTTPAddr

	// 模拟「文件已写、防抖定时器已派发但尚未拿锁」的窗口：
	// 直接 Stop 后再调用 doReload，doReload 应被 watchClosed 守卫短路。
	writeFile(t, path, "server:\n  http_addr: \":5555\"\n")
	Stop()
	doReload() // 关停后仍可能被自身 goroutine 触发；不应改写全局配置

	if got := Get().Server.HTTPAddr; got != want {
		t.Fatalf("关停后不应再热更, got %q want %q", got, want)
	}
}

// ── 热更新（真实 fsnotify + 防抖）─────────────────────────────────────────────

func TestHotReload(t *testing.T) {
	cleanupGlobals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "server:\n  http_addr: \":1111\"\n")

	if err := Init(WithPath(path)); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if got := Get().Server.HTTPAddr; got != ":1111" {
		t.Fatalf("初始 http_addr = %q, want :1111", got)
	}

	reloaded := make(chan struct{}, 1)
	OnReload(func() {
		select {
		case reloaded <- struct{}{}:
		default:
		}
	})

	// 修改文件，触发监听 → 防抖 → 热更。
	writeFile(t, path, "server:\n  http_addr: \":2222\"\n")

	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("修改文件后热更回调未触发")
	}

	if got := Get().Server.HTTPAddr; got != ":2222" {
		t.Fatalf("热更后 http_addr = %q, want :2222", got)
	}
}

func TestHotReloadInvalidKeepsOldConfig(t *testing.T) {
	cleanupGlobals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "server:\n  http_addr: \":3333\"\n")

	if err := Init(WithPath(path), WithWatch(false)); err != nil {
		t.Fatalf("Init error: %v", err)
	}

	// 写入非法 yaml 后直接触发 doReload：应保留旧配置，不 panic。
	writeFile(t, path, "server: [broken\n: : :\n")
	doReload()

	if got := Get().Server.HTTPAddr; got != ":3333" {
		t.Fatalf("热更失败时应保留旧配置, 当前 = %q", got)
	}
}

// ── OnReload 回调 ─────────────────────────────────────────────────────────────

func TestOnReloadNilIgnored(t *testing.T) {
	cleanupGlobals(t)
	OnReload(nil)
	reloadCallbacksMu.RLock()
	n := len(reloadCallbacks)
	reloadCallbacksMu.RUnlock()
	if n != 0 {
		t.Fatalf("OnReload(nil) 不应注册回调, len=%d", n)
	}
}

func TestReloadCallbackPanicIsolated(t *testing.T) {
	cleanupGlobals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "log:\n  level: \"info\"\n")
	if err := Init(WithPath(path), WithWatch(false)); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	_ = captureLog(t) // 吞掉 panic 日志，保持测试输出干净

	var secondCalled bool
	OnReload(func() { panic("boom") })
	OnReload(func() { secondCalled = true })

	// 直接触发回调链；首个回调 panic 不应影响后续回调或导致测试崩溃。
	runReloadCallbacks()

	if !secondCalled {
		t.Fatal("前一个回调 panic 后，后续回调仍应执行")
	}
}

// ── diff 日志 ─────────────────────────────────────────────────────────────────

func TestLogConfigDiff(t *testing.T) {
	cleanupGlobals(t)
	old := []byte("a: 1\nb: 2\nd: 9\n")
	prevRawYAML.Store(&old)

	readLog := captureLog(t)
	newRaw := []byte("a: 1\nb: 3\nc: 4\n") // b 变更、c 新增、d 移除
	logConfigDiff(newRaw)

	out := readLog()
	for _, want := range []string{"配置变更: b", "新增配置项: c", "移除配置项: d"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("diff 日志缺少 %q\n实际输出:\n%s", want, out)
		}
	}
}

// ── 环境解析 ──────────────────────────────────────────────────────────────────

func TestEnvConsistency(t *testing.T) {
	// appEnv 在包初始化时固化，这里只验证 Env 与 IsDev 的一致性。
	if IsDev() != (Env() == "dev") {
		t.Fatalf("IsDev()=%v 与 Env()=%q 不一致", IsDev(), Env())
	}
	if Env() == "" {
		t.Fatal("Env() 不应为空")
	}
}

// ── 并发安全（配合 -race 运行）────────────────────────────────────────────────

func TestConcurrentGetDuringReload(t *testing.T) {
	cleanupGlobals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "server:\n  http_addr: \":8080\"\n")
	if err := Init(WithPath(path), WithWatch(false)); err != nil {
		t.Fatalf("Init error: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					if Get() == nil {
						return
					}
				}
			}
		})
	}

	// 主 goroutine 持续触发热更，与读端并发竞争原子指针。
	for range 100 {
		doReload()
	}
	close(stop)
	wg.Wait()

	if Get() == nil {
		t.Fatal("并发结束后配置不应为 nil")
	}
}
