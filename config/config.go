// Package config 负责应用配置的加载、多环境解析与热更新，完全基于成熟开源库实现
// （gopkg.in/yaml.v3 解析 + github.com/fsnotify/fsnotify 监听），不依赖任何框架私有组件。
//
// 能力：
//   - 基于 APP_ENV 的多环境路径解析：config/app.{env}.yaml，不存在时回退 config/app.yaml
//   - YAML 解析 + ${ENV_VAR} 环境变量展开（适配 CI/CD 与容器部署）
//   - 文件修改后自动热更新（见 watcher.go，带防抖），无需重启
//   - atomic.Pointer 保存快照，config.Get() 读端无锁并发安全
//   - OnReload 热更回调注册，供组件在配置变化后重建内部状态
//   - 顶层 key 变更 diff 日志
package config

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"aura/pkg/log"

	"gopkg.in/yaml.v3"
)

// defaultConfigPath 是未拆分环境时的兜底配置文件路径。
const defaultConfigPath = "config/app.yaml"

// appEnv 是当前运行环境名称，APP_ENV 未设置时默认 "dev"。全局唯一读取 APP_ENV 的地方。
var appEnv = func() string {
	if env := os.Getenv("APP_ENV"); env != "" {
		return env
	}

	return "dev"
}()

// Env 返回当前运行环境名称（如 "dev" / "test" / "prod"）。
func Env() string { return appEnv }

// IsDev 返回当前是否为开发环境（APP_ENV 为空或 "dev"）。
func IsDev() bool { return appEnv == "dev" }

var (
	// globalCfg 原子指针保存配置快照：写端 Store，读端 Load，无锁并发安全。
	globalCfg atomic.Pointer[Config]
	// prevRawYAML 上次加载的原始 yaml 字节，用于热更时对比生成 diff。
	prevRawYAML atomic.Pointer[[]byte]
	// configFilePath 当前生效的配置文件路径。用 atomic.Pointer 保证 Init 写入与
	// watcher 协程读取之间无数据竞争；测试中也可安全清空。
	configFilePath atomic.Pointer[string]
	// initOnce 保证 Init 在生产路径中幂等（测试可通过 resetForTesting 重置）。
	initOnce sync.Once
	// initErr 记录首次 Init 的结果，便于幂等返回。
	initErr error
)

// 热更回调列表。组件在启动期通过 OnReload 注册，doReload 成功后按注册顺序串行执行。
// 本包不反向 import 业务/组件包（避免循环依赖），由各组件主动挂载回调。
var (
	reloadCallbacksMu sync.RWMutex
	reloadCallbacks   []func()
)

// Option 是 Init 的可选配置项。
type Option func(*options)

type options struct {
	path  string
	watch bool
}

// WithPath 显式指定配置文件路径，覆盖基于 APP_ENV 的自动解析（常用于测试）。
func WithPath(path string) Option { return func(o *options) { o.path = path } }

// WithWatch 控制是否启用文件监听热更新（默认启用）。
func WithWatch(enabled bool) Option { return func(o *options) { o.watch = enabled } }

// Init 加载配置并（默认）启动文件监听，通常在程序启动时调用一次。
// 配置文件通过 APP_ENV 选择，修改后自动热更，无需重启。
// 同一进程内多次调用是幂等的（仅首次生效），便于在测试中通过 SetForTesting 等
// 路径绕过；如需在测试中重置内部状态，使用 resetForTesting。
func Init(opts ...Option) error {
	initOnce.Do(func() {
		initErr = doInit(opts...)
	})
	return initErr
}

func doInit(opts ...Option) error {
	o := options{watch: true}
	for _, fn := range opts {
		fn(&o)
	}

	path := o.path
	if path == "" {
		path = resolveConfigPath()
	}

	cfg, raw, err := loadConfig(path)
	if err != nil {
		return err
	}

	pathCopy := path
	configFilePath.Store(&pathCopy)
	globalCfg.Store(cfg)
	prevRawYAML.Store(&raw)

	if o.watch {
		if err := startWatch(path); err != nil {
			// 监听启动失败不影响已加载配置，仅丢失热更能力，降级为警告。
			log.Warnf("[Config] 启动文件监听失败，热更新不可用: %v", err)
		}
	}

	log.Infof("[Config] 配置加载完成 (env=%s, path=%s)", appEnv, path)
	return nil
}

// resolveConfigPath 优先返回 config/app.{APP_ENV}.yaml，不存在时回退 config/app.yaml。
func resolveConfigPath() string {
	envPath := fmt.Sprintf("config/app.%s.yaml", appEnv)
	if _, err := os.Stat(envPath); err == nil {
		return envPath
	}
	log.Warnf("[Config] 未找到 %s，回退到 %s", envPath, defaultConfigPath)
	return defaultConfigPath
}

// loadConfig 读取并解析配置文件，返回配置快照与原始字节；解析前做 ${ENV_VAR} 展开。
func loadConfig(path string) (*Config, []byte, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path 由 APP_ENV 推导或显式传入，非外部不可信输入
	if err != nil {
		return nil, nil, fmt.Errorf("config: 读取文件 %q 失败: %w", path, err)
	}

	cfg := newDefaultConfig()
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(raw))), cfg); err != nil {
		return nil, nil, fmt.Errorf("config: 解析 %q 失败: %w", path, err)
	}
	return cfg, raw, nil
}

// doReload 重新加载配置并原子替换全局快照；解析失败时保留旧配置不中断服务。
// 由 watcher 在文件变更（防抖后）触发。Stop 之后调用为 no-op，避免关停期改写状态。
func doReload() {
	if watchClosed.Load() {
		return
	}
	pathPtr := configFilePath.Load()
	if pathPtr == nil {
		return
	}
	cfg, raw, err := loadConfig(*pathPtr)
	if err != nil {
		log.Warnf("[Config] 热更新失败，继续使用旧配置: %v", err)
		return
	}

	globalCfg.Store(cfg)
	log.Info("[Config] 配置已热更新")

	logConfigDiff(raw)
	runReloadCallbacks()
}

// logConfigDiff 将本次原始 yaml 与上次内容做顶层 key diff 并打印。
func logConfigDiff(newRaw []byte) {
	oldPtr := prevRawYAML.Swap(&newRaw)
	if oldPtr == nil {
		return
	}

	var oldMap, newMap map[string]any
	if yaml.Unmarshal(*oldPtr, &oldMap) != nil || yaml.Unmarshal(newRaw, &newMap) != nil {
		return
	}

	for key, newVal := range newMap {
		switch oldVal, exists := oldMap[key]; {
		case !exists:
			log.Infof("[Config] 新增配置项: %s = %v", key, newVal)
		case fmt.Sprintf("%v", oldVal) != fmt.Sprintf("%v", newVal):
			log.Infof("[Config] 配置变更: %s | %v → %v", key, oldVal, newVal)
		}
	}

	for key, oldVal := range oldMap {
		if _, exists := newMap[key]; !exists {
			log.Infof("[Config] 移除配置项: %s (原值: %v)", key, oldVal)
		}
	}
}

// OnReload 注册一个配置热更后回调。回调按注册顺序串行执行，单个 panic 不影响其他回调或主流程。
// 典型用途：组件根据 yaml 字段重建内部状态（如重建 Redis / HTTP 客户端）。
func OnReload(fn func()) {
	if fn == nil {
		return
	}

	reloadCallbacksMu.Lock()
	reloadCallbacks = append(reloadCallbacks, fn)
	reloadCallbacksMu.Unlock()
}

func runReloadCallbacks() {
	reloadCallbacksMu.RLock()
	cbs := make([]func(), len(reloadCallbacks))
	copy(cbs, reloadCallbacks)
	reloadCallbacksMu.RUnlock()

	for _, cb := range cbs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Errorf("[Config] 热更回调 panic: %v", r)
				}
			}()
			cb()
		}()
	}
}

// Get 返回当前生效的配置快照（原子读，无锁，并发安全）。
// 调用方不应修改返回内容；如需变更请走配置文件热更流程。
func Get() *Config { return globalCfg.Load() }

// SetForTesting 仅供单元测试覆盖全局配置。
func SetForTesting(cfg *Config) {
	if cfg != nil {
		globalCfg.Store(cfg)
	}
}

// resetForTesting 重置所有包级单例与状态，仅供单元测试使用。
// 用于在多个测试用例中重新调用 Init / 验证幂等边界。
func resetForTesting() {
	Stop()
	initOnce = sync.Once{}
	initErr = nil
	globalCfg.Store(nil)
	prevRawYAML.Store(nil)
	configFilePath.Store(nil)
	// Stop 会把 watchClosed 置 true，下一轮测试可能直接调用 doReload / Init(WithWatch(false))，
	// 因此重置回 false，使非监听场景仍能正常 reload。
	watchClosed.Store(false)
}

// Stop 停止文件监听并释放资源，通常在程序优雅退出时调用。
func Stop() { stopWatch() }
