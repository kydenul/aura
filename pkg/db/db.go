// Package db 提供与框架解耦的关系型数据库组件（MySQL / PostgreSQL），封装 GORM 单例。
//
// 设计要点：
//   - 单例 + 包级便捷函数：业务代码 import 本包即用 db.Get() 拿到 *gorm.DB。
//   - 零依赖业务 / config 包：本包仅依赖 GORM，不反向 import config，避免循环依赖；
//     由 main 在启动期用 config 的值调用 Init 完成装配（对齐 pkg/log、pkg/otel）。
//   - 连接池参数显式可配：最大连接 / 空闲连接 / 连接存活时间由调用方传入。
//   - OTel 集成可选：main 在启用 tracing 后调用 InstrumentTracing 挂 otelgorm 插件，
//     把每次 SQL 调用纳入当前 trace（依赖业务侧使用 db.Get().WithContext(ctx) 传播 span）。
package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/uptrace/opentelemetry-go-extra/otelgorm"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"aura/pkg/log"
)

// 支持的数据库驱动。
const (
	DriverMySQL    = "mysql"    // MySQL / MariaDB
	DriverPostgres = "postgres" // PostgreSQL
)

// 连接池默认值（Options 对应字段 <=0 时回退）。
const (
	defaultMaxOpenConns    = 50
	defaultMaxIdleConns    = 10
	defaultConnMaxLifetime = 30 * time.Minute
	defaultConnMaxIdleTime = 10 * time.Minute
)

// Options 数据库连接参数，字段与 config.DatabaseConfig 对应，由 main 在启动期填充。
type Options struct {
	// Driver 数据库驱动：mysql | postgres，留空按 mysql 处理。
	Driver string
	// DSN 数据源名称：
	//   - mysql:    user:pass@tcp(host:3306)/db?charset=utf8mb4&parseTime=true&loc=Local
	//   - postgres: host=h user=u password=p dbname=d port=5432 sslmode=disable TimeZone=Asia/Shanghai
	//               （亦支持 URL 形式 postgres://user:pass@host:5432/db?sslmode=disable）
	DSN string
	// MaxOpenConns 最大打开连接数，<=0 时取默认值。
	MaxOpenConns int
	// MaxIdleConns 最大空闲连接数，<=0 时取默认值。
	MaxIdleConns int
	// ConnMaxLifetime 连接最大存活时间，<=0 时取默认值。
	ConnMaxLifetime time.Duration
}

var globalDB *gorm.DB

// Init 初始化全局数据库连接（建连接池 + Ping 校验），失败返回 error 由 main 决定是否 Fatal。
func Init(opts Options) error {
	gdb, err := open(opts)
	if err != nil {
		return err
	}
	globalDB = gdb
	return nil
}

// Get 返回全局 *gorm.DB 实例；未初始化时返回 nil。
func Get() *gorm.DB {
	return globalDB
}

// SQLDB 返回底层 *sql.DB，用于优雅关闭、连接池操作或 /readyz 探针 PingContext。
// 未初始化时返回 nil。GORM v2 在正常状态下 .DB() 不会失败，
// 但若底层 dialector 出现异常这里仍兜底打错误日志便于排障。
func SQLDB() *sql.DB {
	if globalDB == nil {
		return nil
	}
	sqlDB, err := globalDB.DB()
	if err != nil {
		log.Errorf("db.SQLDB: 获取底层 *sql.DB 失败: %v", err)
		return nil
	}
	return sqlDB
}

// Close 关闭底层连接池，供优雅退出调用。
func Close() error {
	sqlDB := SQLDB()
	if sqlDB == nil {
		return nil
	}
	return sqlDB.Close()
}

// InjectTestDB 仅用于测试：替换全局 DB 实例，传 nil 可恢复。
func InjectTestDB(testDB *gorm.DB) {
	globalDB = testDB
}

// InstrumentTracing 给全局 *gorm.DB 挂上 otelgorm 插件，把 SQL 调用纳入当前 trace
// （span 名为 SQL 语句、记录耗时与影响行数等属性）。重复调用 / 未初始化时静默 no-op。
//
// 默认会注册 DBStats 相关指标到全局 MeterProvider；如不需要指标可改为传入
// `otelgorm.WithoutMetrics()`。本封装故意不暴露选项，保持 main 侧调用极简。
//
// 业务侧若需要把 SQL 挂到上游 span，必须用 `db.Get().WithContext(ctx).Find(...)`
// 显式传 ctx；不带 ctx 的调用仍能产出独立 span，但无法续在父 trace 上。
func InstrumentTracing() {
	if globalDB == nil {
		return
	}
	if err := globalDB.Use(otelgorm.NewPlugin()); err != nil {
		log.Warnf("db.InstrumentTracing: 挂 otelgorm 插件失败: %v", err)
	}
}

func open(opts Options) (*gorm.DB, error) {
	dialector, err := dialector(opts.Driver, opts.DSN)
	if err != nil {
		return nil, err
	}

	gdb, err := gorm.Open(dialector, &gorm.Config{
		// GORM 日志交由统一 zap 体系；这里静默 GORM 自带的标准输出 logger。
		Logger: logger.Default.LogMode(logger.Silent),
		NowFunc: func() time.Time {
			return time.Now().Local()
		},
	})
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("获取数据库连接失败: %w", err)
	}

	sqlDB.SetMaxOpenConns(intOrDefault(opts.MaxOpenConns, defaultMaxOpenConns))
	sqlDB.SetMaxIdleConns(intOrDefault(opts.MaxIdleConns, defaultMaxIdleConns))
	sqlDB.SetConnMaxLifetime(durationOrDefault(opts.ConnMaxLifetime, defaultConnMaxLifetime))
	sqlDB.SetConnMaxIdleTime(defaultConnMaxIdleTime)

	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("数据库连接测试失败: %w", err)
	}

	return gdb, nil
}

// dialector 按驱动名选择 GORM dialector；driver 留空按 mysql 处理。
func dialector(driver, dsn string) (gorm.Dialector, error) {
	switch driver {
	case "", DriverMySQL:
		return mysql.Open(dsn), nil
	case DriverPostgres:
		return postgres.Open(dsn), nil
	default:
		return nil, fmt.Errorf("不支持的数据库驱动: %q（仅支持 %s / %s）", driver, DriverMySQL, DriverPostgres)
	}
}

func intOrDefault(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func durationOrDefault(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}
