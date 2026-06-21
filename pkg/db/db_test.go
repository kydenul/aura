package db

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// ======================== 纯函数：默认值回退 ========================

func TestIntOrDefault(t *testing.T) {
	cases := []struct {
		v, fallback, want int
	}{
		{0, 50, 50},  // 0 回退
		{-1, 50, 50}, // 负数回退
		{25, 50, 25}, // 正数原样
		{1, 50, 1},   // 边界正数
	}
	for _, tc := range cases {
		if got := intOrDefault(tc.v, tc.fallback); got != tc.want {
			t.Errorf("intOrDefault(%d, %d)=%d, 期望 %d", tc.v, tc.fallback, got, tc.want)
		}
	}
}

func TestDurationOrDefault(t *testing.T) {
	fallback := 30 * time.Minute
	cases := []struct {
		v, want time.Duration
	}{
		{0, fallback},            // 0 回退
		{-time.Second, fallback}, // 负数回退
		{time.Hour, time.Hour},   // 正数原样
	}
	for _, tc := range cases {
		if got := durationOrDefault(tc.v, fallback); got != tc.want {
			t.Errorf("durationOrDefault(%v)=%v, 期望 %v", tc.v, got, tc.want)
		}
	}
}

// ======================== dialector：驱动选择 ========================

func TestDialector_Drivers(t *testing.T) {
	cases := []struct {
		name     string
		driver   string
		wantName string // gorm.Dialector.Name()
	}{
		{"空字符串默认 mysql", "", "mysql"},
		{"显式 mysql", DriverMySQL, "mysql"},
		{"postgres", DriverPostgres, "postgres"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := dialector(tc.driver, "dsn-placeholder")
			if err != nil {
				t.Fatalf("dialector(%q) 不应报错: %v", tc.driver, err)
			}
			if d == nil {
				t.Fatalf("dialector(%q) 返回 nil", tc.driver)
			}
			if d.Name() != tc.wantName {
				t.Errorf("driver=%q 期望 dialector.Name()=%q, 得 %q", tc.driver, tc.wantName, d.Name())
			}
		})
	}
}

func TestDialector_UnsupportedDriver(t *testing.T) {
	d, err := dialector("oracle", "dsn")
	if err == nil {
		t.Fatal("非法驱动应返回错误")
	}
	if d != nil {
		t.Errorf("非法驱动应返回 nil dialector，得 %v", d)
	}
}

// ======================== 全局单例：未初始化时的安全行为 ========================

func TestGlobal_UninitializedSafe(t *testing.T) {
	// 确保从干净状态开始（其他测试可能注入过）。
	InjectTestDB(nil)
	t.Cleanup(func() { InjectTestDB(nil) })

	if Get() != nil {
		t.Error("未初始化时 Get() 应返回 nil")
	}
	if SQLDB() != nil {
		t.Error("未初始化时 SQLDB() 应返回 nil")
	}
	if err := Close(); err != nil {
		t.Errorf("未初始化时 Close() 不应报错，得 %v", err)
	}
}

// ======================== InjectTestDB + Get/SQLDB/Close ========================

// newMockGormDB 用 go-sqlmock 构造一个不真正连库的 *gorm.DB。
// SkipInitializeWithVersion 避免 gorm 启动时探测 DB 版本（mock 无需真实连接）。
func newMockGormDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("创建 sqlmock 失败: %v", err)
	}
	gdb, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open(mock) 失败: %v", err)
	}
	return gdb, mock
}

func TestInjectTestDB_GetAndSQLDB(t *testing.T) {
	gdb, _ := newMockGormDB(t)
	InjectTestDB(gdb)
	t.Cleanup(func() { InjectTestDB(nil) })

	if Get() != gdb {
		t.Error("Get() 应返回注入的实例")
	}
	if SQLDB() == nil {
		t.Error("注入后 SQLDB() 不应为 nil")
	}
}

func TestClose_ClosesUnderlyingPool(t *testing.T) {
	gdb, mock := newMockGormDB(t)
	InjectTestDB(gdb)
	t.Cleanup(func() { InjectTestDB(nil) })

	mock.ExpectClose()
	if err := Close(); err != nil {
		t.Fatalf("Close() 不应报错: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("底层连接池未被关闭: %v", err)
	}
}
