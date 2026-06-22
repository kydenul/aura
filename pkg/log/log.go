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
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// 支持的日志输出格式。
const (
	FormatText = "text" // 控制台可读格式（彩色级别），适合本地开发
	FormatJSON = "json" // 结构化 JSON，每行一条，适合生产日志采集
)

// 支持的日志输出目标。
const (
	OutputStdout = "stdout" // 仅写标准输出（云原生 / 容器默认，滚动交给采集侧）
	OutputFile   = "file"   // 仅写文件，按大小自动切割（lumberjack），防止单文件无限膨胀
	OutputBoth   = "both"   // 同时写 stdout 与文件
)

// Config 是日志组件的配置项，字段与 config.LogConfig 一一对应。
type Config struct {
	// Level 日志级别：debug | info | warn | error，留空按 info 处理。
	Level string
	// Format 输出格式：text | json，非 json 一律按 text 处理。
	Format string
	// Output 输出目标：stdout（默认）| file | both，非法值按 stdout 处理。
	Output string
	// File 文件滚动参数，仅当 Output 含 file 时生效。
	File FileConfig
}

// FileConfig 是日志文件滚动（rotation）参数，底层由 lumberjack 实现：
// 单文件超过 MaxSizeMB 即切割，并按 MaxBackups / MaxAgeDays 清理旧文件，
// 从而避免日志文件无限膨胀导致磁盘占满 / 查询困难。
type FileConfig struct {
	// Path 日志文件路径，Output 含 file 时必填（为空则退化为仅 stdout）。
	Path string
	// MaxSizeMB 单个日志文件最大体积（MB），超过即触发切割；<=0 时按 lumberjack 默认 100。
	MaxSizeMB int
	// MaxBackups 最多保留的旧日志文件个数；<=0 表示不按个数清理（谨慎，可能堆积）。
	MaxBackups int
	// MaxAgeDays 旧日志文件最长保留天数；<=0 表示不按时间清理。
	MaxAgeDays int
	// Compress 是否用 gzip 压缩切割后的旧日志文件，省磁盘。
	Compress bool
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
	// fileCloser 持有当前文件输出（lumberjack）的句柄，重建 logger 时关闭旧文件，
	// 避免句柄泄漏；stdout 输出时为 nil。
	fileCloser io.Closer
)

// init 在 Init 被调用前提供一个安全可用的默认 logger（text/info/stdout），
// 避免任何早于 Init 的日志调用触发空指针。
func init() {
	l, _ := build(Config{Format: FormatText, Output: OutputStdout}, atomicLevel)
	store(l, nil)
}

// Init 根据配置初始化全局日志组件，通常在程序启动时调用一次。
// 重复调用会按新配置重建 logger（如 format / output 变化），并发安全。
func Init(cfg Config) error {
	lvl, err := parseLevel(cfg.Level)
	if err != nil {
		return err
	}
	atomicLevel.SetLevel(lvl)
	l, closer := build(cfg, atomicLevel)
	store(l, closer)

	// 启动期打印日志落点：写文件时给出绝对路径（相对路径已按 cwd 解析），
	// 仅 stdout 时显式提示未写文件，避免「找不到日志文件」的困惑。
	p, _ := resolveFileOutput(cfg)
	if p != "" {
		l.Info("📁 日志文件: " + p)
	} else {
		l.Info("📑 日志仅输出到 stdout，未写入文件")
	}

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

// build 按配置构造 zap.Logger 及（可选的）文件输出句柄：
//   - 编码器按 format 选择 console（text）或 json；
//   - 写入目标按 output 选择 stdout / 文件（lumberjack 滚动）/ 二者并写；
//   - 级别由 atomicLevel 动态控制，支持热更。
//
// 返回的 io.Closer 为文件输出句柄（stdout-only 时为 nil），由 store 负责在
// 替换 logger 时关闭旧句柄。
func build(cfg Config, lvl zap.AtomicLevel) (*zap.Logger, io.Closer) {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.MessageKey = "msg"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeDuration = zapcore.StringDurationEncoder

	ws, closer := buildWriteSyncer(cfg)
	// 彩色级别只在「纯 stdout」时启用：写文件时 ANSI 转义码会污染日志、干扰检索。
	colorize := closer == nil

	var encoder zapcore.Encoder
	if strings.EqualFold(cfg.Format, FormatJSON) {
		encCfg.EncodeLevel = zapcore.LowercaseLevelEncoder
		encoder = zapcore.NewJSONEncoder(encCfg)
	} else {
		// text：控制台编码器，人类可读，便于本地排查。
		if colorize {
			encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		} else {
			encCfg.EncodeLevel = zapcore.CapitalLevelEncoder
		}
		encoder = zapcore.NewConsoleEncoder(encCfg)
	}

	core := zapcore.NewCore(encoder, ws, lvl)
	// error 及以上自动附带堆栈，方便定位。
	return zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel)), closer
}

// buildWriteSyncer 按 output 决定日志写入目标：
//   - file / both：经 lumberjack 写文件，单文件超阈值自动切割并按个数/天数清理；
//   - 其余（含非法值或文件路径为空）：退化为仅 stdout，保证日志永不丢失。
func buildWriteSyncer(cfg Config) (zapcore.WriteSyncer, io.Closer) {
	path, ok := resolveFileOutput(cfg)
	if !ok {
		return zapcore.Lock(os.Stdout), nil
	}

	lj := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    cfg.File.MaxSizeMB,
		MaxBackups: cfg.File.MaxBackups,
		MaxAge:     cfg.File.MaxAgeDays,
		Compress:   cfg.File.Compress,
		LocalTime:  true,
	}

	if strings.EqualFold(cfg.Output, OutputBoth) {
		ws := zapcore.NewMultiWriteSyncer(zapcore.Lock(os.Stdout), zapcore.AddSync(lj))
		return ws, lj
	}
	return zapcore.AddSync(lj), lj
}

// resolveFileOutput 判定给定配置下是否启用文件输出，并返回最终落盘的绝对路径。
// 仅当 output 含 file / both 且 path 非空时启用；相对路径按进程当前工作目录（cwd）
// 解析为绝对路径，使「实际写入位置」与「启动期打印的路径」严格一致，避免因启动目录
// 不同而找不到文件。Abs 失败（极少见）时退回原始路径，保证不影响写入。
func resolveFileOutput(cfg Config) (string, bool) {
	wantFile := strings.EqualFold(cfg.Output, OutputFile) || strings.EqualFold(cfg.Output, OutputBoth)
	p := strings.TrimSpace(cfg.File.Path)
	if !wantFile || p == "" {
		return "", false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p, true
	}
	return abs, true
}

// store 原子替换全局 logger 及其衍生实例，并关闭被替换掉的旧文件句柄。
func store(l *zap.Logger, closer io.Closer) {
	mu.Lock()
	defer mu.Unlock()
	old := fileCloser
	base = l
	baseSugar = l.Sugar()
	skip := l.WithOptions(zap.AddCallerSkip(1))
	skipLog = skip
	skipSugar = skip.Sugar()
	fileCloser = closer
	if old != nil && old != closer {
		_ = old.Close()
	}
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
	store(zap.New(core), nil)

	return func() {
		mu.Lock()
		base, baseSugar = prevBase, prevBaseSugar
		skipLog, skipSugar = prevSkip, prevSkipSugar
		mu.Unlock()
	}
}
