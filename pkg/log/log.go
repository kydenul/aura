// Package log 提供一个与框架解耦的结构化日志组件，封装业界事实标准
// go.uber.org/zap（craftia-admin 后端使用的 tRPC log 底层亦为 zap）。
//
// 设计要点：
//   - 单例 + 包级便捷函数：业务代码 import 本包即用，无需层层传递 logger。
//   - level / format 由 config 驱动：format 支持 text（控制台彩色，便于本地开发）
//     与 json（结构化，便于生产采集）。
//   - 级别热更：内部用 zap.AtomicLevel 持有级别，SetLevel 可在不重建 logger 的
//     前提下动态调整，配合 config 热更回调即可改 yaml 立即生效（对应 LogConfig 的
//     【热更生效】约定）。
//   - 零依赖业务包：本包仅依赖 zap，不反向 import config / 业务包，避免循环依赖；
//     由 main 在启动期用 config 的值调用 Init 完成装配。
package log

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// 支持的日志输出格式。
const (
	FormatText = "text" // 控制台可读格式（彩色级别），适合本地开发
	FormatJSON = "json" // 结构化 JSON，每行一条，适合生产日志采集
)

// Config 是日志组件的配置项，字段与 config.LogConfig 一一对应。
type Config struct {
	// Level 日志级别：debug | info | warn | error，留空按 info 处理。
	Level string
	// Format 输出格式：text | json，非 json 一律按 text 处理。
	Format string
}

// Field 是结构化日志字段别名，调用方统一从本包引用，无需直接 import zap。
type Field = zap.Field

// 重新导出最常用的字段构造器，覆盖绝大多数业务场景；
// 需要更多类型时仍可直接使用 zap.XXX。
var (
	String   = zap.String
	Int      = zap.Int
	Int32    = zap.Int32
	Int64    = zap.Int64
	Uint64   = zap.Uint64
	Float64  = zap.Float64
	Bool     = zap.Bool
	Duration = zap.Duration
	Time     = zap.Time
	Any      = zap.Any
	Err      = zap.Error
	Stack    = zap.Stack
)

var (
	mu sync.RWMutex
	// base 提供给 L() 直接使用，调用栈 skip 为 0。
	base *zap.Logger
	// baseSugar 是 base 的 Sugar 版本，供 S() 使用。
	baseSugar *zap.SugaredLogger
	// skipLog 供包级便捷函数（Info/Warn/...）使用，多跳过一层包装栈，
	// 使日志里的 caller 指向真正的调用点而非本文件。
	skipLog *zap.Logger
	// skipSugar 供包级格式化便捷函数（Infof/...）使用。
	skipSugar *zap.SugaredLogger
	// atomicLevel 持有当前级别，支持运行期热更而无需重建 logger。
	atomicLevel = zap.NewAtomicLevelAt(zapcore.InfoLevel)
)

// init 在 Init 被调用前提供一个安全可用的默认 logger（text/info），
// 避免任何早于 Init 的日志调用触发空指针。
func init() {
	store(build(FormatText, atomicLevel))
}

// Init 根据配置初始化全局日志组件，通常在程序启动时调用一次。
// 重复调用会按新配置重建 logger（如 format 变化），并发安全。
func Init(cfg Config) error {
	lvl, err := parseLevel(cfg.Level)
	if err != nil {
		return err
	}
	atomicLevel.SetLevel(lvl)
	store(build(cfg.Format, atomicLevel))
	return nil
}

// SetLevel 仅热更日志级别（不重建 logger），供 config 热更回调调用。
func SetLevel(level string) error {
	lvl, err := parseLevel(level)
	if err != nil {
		return err
	}
	atomicLevel.SetLevel(lvl)
	return nil
}

// build 按 format 构造一个写到 stdout 的 zap.Logger，级别由 atomicLevel 动态控制。
func build(format string, lvl zap.AtomicLevel) *zap.Logger {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.MessageKey = "msg"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeDuration = zapcore.StringDurationEncoder

	var encoder zapcore.Encoder
	if strings.EqualFold(format, FormatJSON) {
		encCfg.EncodeLevel = zapcore.LowercaseLevelEncoder
		encoder = zapcore.NewJSONEncoder(encCfg)
	} else {
		// text：控制台编码器 + 彩色大写级别，人类可读，便于本地排查。
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		encoder = zapcore.NewConsoleEncoder(encCfg)
	}

	core := zapcore.NewCore(encoder, zapcore.Lock(os.Stdout), lvl)
	// error 及以上自动附带堆栈，方便定位。
	return zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
}

// store 原子替换全局 logger 及其衍生实例。
func store(l *zap.Logger) {
	mu.Lock()
	defer mu.Unlock()
	base = l
	baseSugar = l.Sugar()
	skip := l.WithOptions(zap.AddCallerSkip(1))
	skipLog = skip
	skipSugar = skip.Sugar()
}

// parseLevel 把字符串级别解析为 zapcore.Level，留空按 info 处理。
func parseLevel(s string) (zapcore.Level, error) {
	if strings.TrimSpace(s) == "" {
		return zapcore.InfoLevel, nil
	}
	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(strings.ToLower(strings.TrimSpace(s)))); err != nil {
		return zapcore.InfoLevel, fmt.Errorf("log: 非法日志级别 %q: %w", s, err)
	}
	return lvl, nil
}

// L 返回底层 *zap.Logger，需要 zap 原生能力（如 With 派生子 logger）时使用。
func L() *zap.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return base
}

// S 返回底层 *zap.SugaredLogger，适合 printf 风格或不在意性能的场景。
func S() *zap.SugaredLogger {
	mu.RLock()
	defer mu.RUnlock()
	return baseSugar
}

// logger / sugar 取出供包级便捷函数使用的实例（已多 skip 一层栈）。
func logger() *zap.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return skipLog
}

func sugar() *zap.SugaredLogger {
	mu.RLock()
	defer mu.RUnlock()
	return skipSugar
}

// 包级结构化便捷函数（key/value 字段，零分配、高性能）。

// Debug 记录 debug 级别日志。
func Debug(msg string, fields ...Field) { logger().Debug(msg, fields...) }

// Info 记录 info 级别日志。
func Info(msg string, fields ...Field) { logger().Info(msg, fields...) }

// Warn 记录 warn 级别日志。
func Warn(msg string, fields ...Field) { logger().Warn(msg, fields...) }

// Error 记录 error 级别日志（自动附带堆栈）。
func Error(msg string, fields ...Field) { logger().Error(msg, fields...) }

// Fatal 记录 fatal 级别日志后调用 os.Exit(1)，仅用于启动期不可恢复错误。
func Fatal(msg string, fields ...Field) { logger().Fatal(msg, fields...) }

// 包级格式化便捷函数（printf 风格），用于平滑替换标准库 log。

// Debugf 以 printf 风格记录 debug 日志。
func Debugf(format string, args ...any) { sugar().Debugf(format, args...) }

// Infof 以 printf 风格记录 info 日志。
func Infof(format string, args ...any) { sugar().Infof(format, args...) }

// Warnf 以 printf 风格记录 warn 日志。
func Warnf(format string, args ...any) { sugar().Warnf(format, args...) }

// Errorf 以 printf 风格记录 error 日志。
func Errorf(format string, args ...any) { sugar().Errorf(format, args...) }

// Fatalf 以 printf 风格记录 fatal 日志后调用 os.Exit(1)。
func Fatalf(format string, args ...any) { sugar().Fatalf(format, args...) }

// Sync 刷新缓冲到底层（stdout），通常在程序优雅退出时调用。
// stdout 上 Sync 可能返回平台相关的无害错误，调用方一般可忽略。
func Sync() error {
	mu.RLock()
	defer mu.RUnlock()
	return base.Sync()
}

// SetOutputForTesting 仅供单元测试：把全局日志重定向到 w（console 编码，保留当前级别），
// 返回的 restore 函数会还原到调用前的 logger。
func SetOutputForTesting(w io.Writer) (restore func()) {
	mu.Lock()
	prevBase, prevBaseSugar := base, baseSugar
	prevSkip, prevSkipSugar := skipLog, skipSugar
	mu.Unlock()

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	core := zapcore.NewCore(zapcore.NewConsoleEncoder(encCfg), zapcore.AddSync(w), atomicLevel)
	store(zap.New(core))

	return func() {
		mu.Lock()
		base, baseSugar = prevBase, prevBaseSugar
		skipLog, skipSugar = prevSkip, prevSkipSugar
		mu.Unlock()
	}
}
