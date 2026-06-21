// Package redis 提供与框架解耦的 Redis 客户端组件，封装 go-redis v9。
//
// 设计要点：
//   - 单例 + 类型封装：Client 屏蔽底层 go-redis 类型，业务侧 import 本包即用。
//   - 兼容 Codis / 腾讯云 Proxy：显式配置 ConnMaxIdleTime / ConnMaxLifetime / 超时 / 重试，
//     规避 Proxy 默认 600s 空闲断连与主备切换后的僵尸连接问题。
//   - 零依赖业务 / config 包：仅依赖 go-redis，不反向 import config，避免循环依赖；
//     由 main 在启动期用 config 的值调用 Init 完成装配（对齐 pkg/log、pkg/otel）。
//   - OTel 集成可选：main 在启用 tracing / metrics 时调用 InstrumentTracing /
//     InstrumentMetrics，把 Redis 调用纳入当前 trace 和 RED 指标。
package redis

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"

	"aura/pkg/log"
)

// 连接参数默认值（Options 对应字段 <=0 时回退）。
const (
	defaultPoolSize     = 10
	defaultMinIdleConns = 2
	// connMaxIdleTime 空闲连接被回收的阈值。固定值即可：业务侧未暴露成 yaml 配置（已被默认值覆盖大多数场景），
	// 显式设定主要为规避 Codis / 腾讯云 Proxy 默认 600s 空闲断连。
	connMaxIdleTime = 5 * time.Minute
	// connMaxLifetime 单条连接的最大存活时间，避免主备切换后僵尸连接长期占用 pool。
	connMaxLifetime     = 30 * time.Minute
	defaultDialTimeout  = 5 * time.Second
	defaultReadTimeout  = 3 * time.Second
	defaultWriteTimeout = 3 * time.Second
	defaultMaxRetries   = 2
)

// Options Redis 连接参数，字段与 config.RedisConfig 对应，由 main 在启动期填充。
type Options struct {
	Host         string
	Port         int
	Password     string
	DB           int
	PoolSize     int
	MinIdleConns int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxRetries   int
}

// Client Redis 客户端封装。可被多个 goroutine 并发安全使用。
type Client struct {
	client *redis.Client
}

var globalClient *Client

// Init 初始化全局 Redis 客户端（建连接池 + Ping 校验），失败返回 error 由 main 决定是否 Fatal。
func Init(opts Options) error {
	c, err := NewClient(opts)
	if err != nil {
		return err
	}
	globalClient = c
	return nil
}

// Get 获取全局 Redis 客户端实例；未初始化时返回 nil。
func Get() *Client {
	return globalClient
}

// IsInitialized 检查全局 Redis 客户端是否已初始化。
func IsInitialized() bool {
	return globalClient != nil
}

// NewClient 创建一个 Redis 客户端（不写入全局单例）。
func NewClient(opts Options) (*Client, error) {
	addr := fmt.Sprintf("%s:%d", opts.Host, opts.Port)

	redisOpts := &redis.Options{
		Addr:            addr,
		Password:        opts.Password,
		DB:              opts.DB,
		PoolSize:        intOrDefault(opts.PoolSize, defaultPoolSize),
		MinIdleConns:    intOrDefault(opts.MinIdleConns, defaultMinIdleConns),
		ConnMaxIdleTime: connMaxIdleTime,
		ConnMaxLifetime: connMaxLifetime,
		DialTimeout:     durationOrDefault(opts.DialTimeout, defaultDialTimeout),
		ReadTimeout:     durationOrDefault(opts.ReadTimeout, defaultReadTimeout),
		WriteTimeout:    durationOrDefault(opts.WriteTimeout, defaultWriteTimeout),
		MaxRetries:      intOrDefault(opts.MaxRetries, defaultMaxRetries),
	}

	rdb := redis.NewClient(redisOpts)
	log.Infof(
		"Redis 地址: %s, DB: %d, PoolSize: %d, MinIdleConns: %d, ConnMaxIdleTime: %s, ConnMaxLifetime: %s",
		addr, opts.DB, redisOpts.PoolSize, redisOpts.MinIdleConns,
		redisOpts.ConnMaxIdleTime, redisOpts.ConnMaxLifetime,
	)

	// 测试连接（带重试，容忍 Proxy 偶发主备切换）。
	if err := pingWithRetry(rdb, redisOpts.DialTimeout+redisOpts.ReadTimeout, 3); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("连接 Redis 失败: %w", err)
	}
	log.Infof("Redis 连接成功: %s", addr)

	return &Client{client: rdb}, nil
}

// NewClientFromConn 用已有的 *redis.Client 构造封装客户端，主要用于单元测试（如 miniredis）。
func NewClientFromConn(rdb *redis.Client) *Client {
	return &Client{client: rdb}
}

// InstrumentTracing 给全局 Client 挂上 redisotel tracing hook：
// 每条 Redis 命令产出一个 span（span 名为命令名，业务上下文 ctx 续在父 trace 上）。
// 未初始化 / 重复调用静默 no-op；hook 注册失败仅 Warn，不影响进程启动。
func InstrumentTracing() {
	if globalClient == nil {
		return
	}
	if err := redisotel.InstrumentTracing(globalClient.client); err != nil {
		log.Warnf("redis.InstrumentTracing: 挂 redisotel tracing hook 失败: %v", err)
	}
}

// InstrumentMetrics 给全局 Client 挂上 redisotel metrics hook：
// 把命令耗时 / 连接池统计写入全局 MeterProvider，自动并入 /metrics。
func InstrumentMetrics() {
	if globalClient == nil {
		return
	}
	if err := redisotel.InstrumentMetrics(globalClient.client); err != nil {
		log.Warnf("redis.InstrumentMetrics: 挂 redisotel metrics hook 失败: %v", err)
	}
}

// Close 关闭 Redis 连接。
func (c *Client) Close() error {
	return c.client.Close()
}

// Ping 探测 Redis 连通性，供健康 / 就绪探针使用；连接正常返回 nil。
func (c *Client) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// pingWithRetry 对 Proxy 主备切换等瞬时不可用做有限重试，避免启动直接失败。
func pingWithRetry(rdb *redis.Client, timeout time.Duration, attempts int) error {
	var lastErr error
	for i := range attempts {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := rdb.Ping(ctx).Err()
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		log.Warnf("Redis Ping 失败 (第 %d/%d 次): %v", i+1, attempts, err)
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	return lastErr
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

// ======================== 通用操作 ========================

// Set 设置键值对。
func (c *Client) Set(ctx context.Context, key string, value any, expiration time.Duration) error {
	return c.client.Set(ctx, key, value, expiration).Err()
}

// Get 获取值；key 不存在时返回 Nil。
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	return c.client.Get(ctx, key).Result()
}

// MGet 批量 GET；返回值长度 == len(keys)，未命中位置为 nil。
func (c *Client) MGet(ctx context.Context, keys ...string) ([]any, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	return c.client.MGet(ctx, keys...).Result()
}

// Del 删除一个或多个键。
// 单实例 / Proxy 直接一次 RTT 完成；集群分片场景由调用方在 key 模板里用 hash tag
// （如 "user:{uid}:profile"）确保参与同一次调用的 key 落在同一 slot，避免 CROSSSLOT。
func (c *Client) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.client.Del(ctx, keys...).Err()
}

// Exists 检查键是否存在，返回存在的 key 数量。
// 集群分片场景同 Del：调用方用 hash tag 保 slot。
func (c *Client) Exists(ctx context.Context, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	return c.client.Exists(ctx, keys...).Result()
}

// Incr 自增。
func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	return c.client.Incr(ctx, key).Result()
}

// Decr 自减。
func (c *Client) Decr(ctx context.Context, key string) (int64, error) {
	return c.client.Decr(ctx, key).Result()
}

// IncrBy 将 key 中储存的数字增加 increment。
func (c *Client) IncrBy(ctx context.Context, key string, value int64) (int64, error) {
	return c.client.IncrBy(ctx, key, value).Result()
}

// Expire 设置过期时间。
func (c *Client) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	return c.client.Expire(ctx, key, expiration).Result()
}

// TTL 获取剩余过期时间。
func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.client.TTL(ctx, key).Result()
}

// ======================== 原子操作 ========================

// SetNX 仅当 key 不存在时设置值（SET if Not eXists）。
func (c *Client) SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error) {
	return c.client.SetNX(ctx, key, value, expiration).Result()
}

// GetDel 获取并删除 key，返回 string 值。
func (c *Client) GetDel(ctx context.Context, key string) (string, error) {
	return c.client.GetDel(ctx, key).Result()
}

// ======================== Hash 操作 ========================

// HSet 设置 Hash 字段。
func (c *Client) HSet(ctx context.Context, key, field string, value any) error {
	return c.client.HSet(ctx, key, field, value).Err()
}

// HGet 获取 Hash 字段。
func (c *Client) HGet(ctx context.Context, key, field string) (string, error) {
	return c.client.HGet(ctx, key, field).Result()
}

// HGetAll 获取所有 Hash 字段。
func (c *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.client.HGetAll(ctx, key).Result()
}

// HDel 删除 Hash 字段。
func (c *Client) HDel(ctx context.Context, key string, fields ...string) error {
	return c.client.HDel(ctx, key, fields...).Err()
}

// ======================== Set 操作 ========================

// SAdd 向集合添加一个或多个成员。
func (c *Client) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	return c.client.SAdd(ctx, key, members...).Result()
}

// SRem 移除集合中一个或多个成员。
func (c *Client) SRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.client.SRem(ctx, key, members...).Result()
}

// SMembers 返回集合中的所有成员。
func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.client.SMembers(ctx, key).Result()
}

// SIsMember 判断成员是否在集合中。
func (c *Client) SIsMember(ctx context.Context, key string, member any) (bool, error) {
	return c.client.SIsMember(ctx, key, member).Result()
}

// ======================== ZSet 操作 ========================

// ZRemRangeByScore 移除有序集合中分数在 [min, max] 之间的成员。
func (c *Client) ZRemRangeByScore(ctx context.Context, key, minScore, maxScore string) (int64, error) {
	return c.client.ZRemRangeByScore(ctx, key, minScore, maxScore).Result()
}

// ZCard 获取有序集合的成员数。
func (c *Client) ZCard(ctx context.Context, key string) (int64, error) {
	return c.client.ZCard(ctx, key).Result()
}

// ZRem 移除有序集合中一个或多个成员。
func (c *Client) ZRem(ctx context.Context, key string, members ...any) error {
	return c.client.ZRem(ctx, key, members...).Err()
}

// ======================== Pub/Sub ========================

// PubSubMessage 封装 Pub/Sub 中收到的单条消息，屏蔽底层库类型。
type PubSubMessage struct {
	Channel string
	Payload string
}

// PubSubSubscriber 封装 go-redis 的 *redis.PubSub，对外屏蔽底层库类型。
type PubSubSubscriber struct {
	inner *redis.PubSub
	//nolint:containedctx // ctx 与订阅生命周期绑定，Channel() goroutine 用于 ReceiveMessage
	ctx  context.Context
	ch   chan *PubSubMessage
	once sync.Once
}

// Receive 阻塞等待订阅确认，通常在 Channel() 之前调用以确保连接已建立。
func (p *PubSubSubscriber) Receive(ctx context.Context) error {
	_, err := p.inner.Receive(ctx)
	return err
}

// Channel 返回只读消息通道。首次调用时懒启动内部接收 goroutine；
// 当订阅 context 取消或 Close() 被调用后，通道自动关闭。
func (p *PubSubSubscriber) Channel() <-chan *PubSubMessage {
	p.once.Do(func() {
		p.ch = make(chan *PubSubMessage, 100)
		go func() {
			defer close(p.ch)
			for {
				msg, err := p.inner.ReceiveMessage(p.ctx)
				if err != nil {
					return
				}
				p.ch <- &PubSubMessage{Channel: msg.Channel, Payload: msg.Payload}
			}
		}()
	})
	return p.ch
}

// Close 取消订阅并关闭底层连接。
func (p *PubSubSubscriber) Close() error {
	return p.inner.Close()
}

// Publish 将消息发布到指定频道。
func (c *Client) Publish(ctx context.Context, channel string, message any) error {
	return c.client.Publish(ctx, channel, message).Err()
}

// PSubscribe 订阅一个或多个符合给定模式的频道，返回 *PubSubSubscriber。
func (c *Client) PSubscribe(ctx context.Context, patterns ...string) *PubSubSubscriber {
	return &PubSubSubscriber{
		inner: c.client.PSubscribe(ctx, patterns...),
		ctx:   ctx,
	}
}

// ======================== 哨兵错误 ========================

// Nil 是 go-redis 在 key 不存在时返回的哨兵错误，等价于 redis.Nil。
var Nil = redis.Nil
