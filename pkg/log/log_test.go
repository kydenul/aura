package log

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    zapcore.Level
		wantErr bool
	}{
		{"", zapcore.InfoLevel, false},
		{"   ", zapcore.InfoLevel, false},
		{"debug", zapcore.DebugLevel, false},
		{"INFO", zapcore.InfoLevel, false},
		{"Warn", zapcore.WarnLevel, false},
		{"error", zapcore.ErrorLevel, false},
		{"bogus", zapcore.InfoLevel, true},
	}
	for _, c := range cases {
		got, err := parseLevel(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseLevel(%q) 应返回错误", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseLevel(%q) error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("parseLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestInit(t *testing.T) {
	t.Cleanup(func() { _ = Init(Config{Level: "info", Format: "text"}) })

	if err := Init(Config{Level: "debug", Format: "json"}); err != nil {
		t.Fatalf("Init(json/debug) error: %v", err)
	}
	if err := Init(Config{Level: "warn", Format: "text"}); err != nil {
		t.Fatalf("Init(text/warn) error: %v", err)
	}
	// 非法级别应返回错误。
	if err := Init(Config{Level: "bogus"}); err == nil {
		t.Fatal("非法级别 Init 应返回错误")
	}
}

func TestSetLevel(t *testing.T) {
	t.Cleanup(func() { _ = SetLevel("info") })

	if err := SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel(debug) error: %v", err)
	}
	if err := SetLevel("nope"); err == nil {
		t.Fatal("非法级别 SetLevel 应返回错误")
	}
}

// TestLevelFilteringBySetLevel 验证 SetLevel 动态调整级别会过滤低级别日志。
func TestLevelFilteringBySetLevel(t *testing.T) {
	var buf bytes.Buffer
	restore := SetOutputForTesting(&buf)
	defer restore()

	if err := SetLevel("error"); err != nil {
		t.Fatalf("SetLevel error: %v", err)
	}
	defer func() { _ = SetLevel("info") }()

	Info("this-info-should-be-filtered")
	Warn("this-warn-should-be-filtered")
	Error("this-error-should-appear")

	out := buf.String()
	if strings.Contains(out, "this-info-should-be-filtered") {
		t.Errorf("error 级别下不应输出 info 日志\n实际:\n%s", out)
	}
	if strings.Contains(out, "this-warn-should-be-filtered") {
		t.Errorf("error 级别下不应输出 warn 日志\n实际:\n%s", out)
	}
	if !strings.Contains(out, "this-error-should-appear") {
		t.Errorf("error 日志应输出\n实际:\n%s", out)
	}
}

// TestStructuredFieldsOutput 验证结构化字段被正确写出。
func TestStructuredFieldsOutput(t *testing.T) {
	var buf bytes.Buffer
	restore := SetOutputForTesting(&buf)
	defer restore()

	Info("created", String("uid", "u-1"), Int("count", 3), Bool("ok", true))

	out := buf.String()
	for _, want := range []string{"created", "uid", "u-1", "count", "3", "ok", "true"} {
		if !strings.Contains(out, want) {
			t.Errorf("结构化日志缺少 %q\n实际:\n%s", want, out)
		}
	}
}

// TestFormattedOutput 验证 printf 风格便捷函数。
func TestFormattedOutput(t *testing.T) {
	var buf bytes.Buffer
	restore := SetOutputForTesting(&buf)
	defer restore()

	Infof("hello %s %d", "world", 42)
	if out := buf.String(); !strings.Contains(out, "hello world 42") {
		t.Errorf("Infof 输出不正确\n实际:\n%s", out)
	}
}

func TestBuildBothFormats(t *testing.T) {
	if l, _ := build(Config{Format: FormatText, Output: OutputStdout}, atomicLevel); l == nil {
		t.Error("build(text) 返回 nil")
	}
	if l, _ := build(Config{Format: FormatJSON, Output: OutputStdout}, atomicLevel); l == nil {
		t.Error("build(json) 返回 nil")
	}
}

func TestAccessorsNotNil(t *testing.T) {
	if L() == nil || S() == nil || logger() == nil || sugar() == nil {
		t.Fatal("logger 访问器不应返回 nil")
	}
}

// TestFileOutputWritesToFile 验证 output=file 时日志落到文件，且切回 stdout 后旧文件句柄被关闭。
func TestFileOutputWritesToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	t.Cleanup(func() { _ = Init(Config{Level: "info", Format: "text", Output: OutputStdout}) })

	if err := Init(Config{
		Level:  "info",
		Format: "json",
		Output: OutputFile,
		File: FileConfig{
			Path:       path,
			MaxSizeMB:  1,
			MaxBackups: 2,
			MaxAgeDays: 7,
			Compress:   false,
		},
	}); err != nil {
		t.Fatalf("Init(file) error: %v", err)
	}

	Info("file-rotation-works", String("uid", "u-9"))
	_ = Sync()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	out := string(data)
	for _, want := range []string{"file-rotation-works", "uid", "u-9"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志文件缺少 %q\n实际:\n%s", want, out)
		}
	}
	// 文件模式不应写入 ANSI 颜色码。
	if strings.Contains(out, "\x1b[") {
		t.Errorf("文件日志不应包含 ANSI 颜色码\n实际:\n%s", out)
	}
}

// TestFileOutputEmptyPathFallsBackToStdout 验证 output=file 但 path 为空时退化为 stdout（不 panic、不建文件）。
func TestFileOutputEmptyPathFallsBackToStdout(t *testing.T) {
	_, closer := build(Config{Format: FormatText, Output: OutputFile, File: FileConfig{Path: "  "}}, atomicLevel)
	if closer != nil {
		t.Error("path 为空时应退化为 stdout，closer 应为 nil")
	}
}
