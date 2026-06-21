// Package redis 提供基于 Redis 的滑动窗口限流器。
//
// 算法：精确滑动窗口（ZSet 实现）。每次请求执行同一段 Lua 脚本完成
//  1. 移除窗口外（score < now-window）的旧时间戳；
//  2. 统计当前窗口内剩余条目数；
//  3. 若 < limit 则 ZADD 当前时间戳 + 重置 EXPIRE，返回 1（允许）；
//  4. 否则返回 0（拒绝），不写入新条目。
//
// 整段脚本在 Redis 单线程内原子执行，多实例水平扩展场景下也能保证全局一致。
// 脚本本身由 redis.NewScript 缓存：首次调用走 EVAL 注入到 Redis 端 SCRIPT CACHE，
// 后续调用自动走 EVALSHA，避免每次重复传输 Lua 源码。
package redis

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindowScript 精确滑动窗口限流的 Lua 实现。
//
// KEYS[1] = 限流 key（建议形如 "ratelimit:upload:user:{uid}"）
// ARGV[1] = 窗口长度（毫秒）
// ARGV[2] = 当前时间（毫秒，由调用方传入避免 Redis 节点时钟漂移）
// ARGV[3] = 窗口内允许的最大次数
// ARGV[4] = 唯一 member（由进程内单调序列拼成，保证 ZADD 不去重）
//
// 返回：[allowed(0/1), remaining(>=0)]
//   - allowed=1 时 remaining 是写入本次后剩余可用配额；
//   - allowed=0 时 remaining 恒为 0。
var slidingWindowScript = redis.NewScript(`
local key    = KEYS[1]
local window = tonumber(ARGV[1])
local now    = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local member = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, 0, now - window)

local count = redis.call('ZCARD', key)
if count < limit then
    redis.call('ZADD', key, now, member)
    -- 略大于窗口，保证最后一次请求过期后 key 自动清理；单位：毫秒
    redis.call('PEXPIRE', key, window + 1000)
    return {1, limit - count - 1}
end
return {0, 0}
`)

// ErrRateLimitNotReady 当 Redis 客户端未初始化时返回。
// 调用方应据此决定 fail-open（放行）或 fail-close（拦截）。
var ErrRateLimitNotReady = errors.New("ratelimit: redis client not ready")

// memberSeq 进程内单调递增序列，与时间戳一起拼成 ZSet member。
// atomic.Add 保证并发下唯一，根本上避免「同一毫秒并发请求被 ZADD 去重」吞计数的问题。
var memberSeq atomic.Uint64

// SlidingWindowLimiter 基于 Redis ZSet 的滑动窗口限流器。
// 同一实例可被多个 goroutine 并发调用（无内部状态）。
type SlidingWindowLimiter struct {
	client *Client
	// keyPrefix 限流 key 前缀，区分不同业务场景（如 "ratelimit:upload"）。
	keyPrefix string
	// limit 窗口内允许的最大请求数。
	limit int
	// window 窗口长度。
	window time.Duration
}

// NewSlidingWindowLimiter 构造限流器。
//   - client:    Redis 客户端（推荐传 redis.Get()；若为 nil，调用 Allow 时返回 ErrRateLimitNotReady）；
//   - keyPrefix: 限流 key 前缀，用于区分不同业务（避免同一 user_id 的不同动作互相计数）；
//   - limit:     窗口内允许次数（必须 > 0，否则永远拒绝）；
//   - window:    时间窗口长度（必须 > 0）。
func NewSlidingWindowLimiter(
	client *Client,
	keyPrefix string,
	limit int,
	window time.Duration,
) *SlidingWindowLimiter {
	return &SlidingWindowLimiter{
		client:    client,
		keyPrefix: keyPrefix,
		limit:     limit,
		window:    window,
	}
}

// Allow 检查 identity 是否允许通过限流。
// 返回 (allowed, remaining, err)：
//   - allowed=true  表示通过，并已记账本次请求；remaining 为本次记账后剩余配额；
//   - allowed=false 表示被限流；remaining=0；
//   - err != nil    表示后端 Redis 异常或未就绪；调用方应 fail-open（放行）以避免误伤。
//
// identity 通常是 user_id 字符串；不同业务场景由 keyPrefix 区分。
func (l *SlidingWindowLimiter) Allow(ctx context.Context, identity string) (bool, int, error) {
	if l.client == nil {
		return false, 0, ErrRateLimitNotReady
	}
	if l.limit <= 0 || l.window <= 0 {
		// 配置失效一律拒绝，避免静默放行带来的容量风险。
		return false, 0, nil
	}

	key := l.keyPrefix + ":" + identity
	nowMs := time.Now().UnixMilli()
	windowMs := l.window.Milliseconds()
	member := buildLimiterMember(nowMs)

	res, err := slidingWindowScript.
		Run(ctx, l.client.client, []string{key}, windowMs, nowMs, l.limit, member).
		Result()
	if err != nil {
		return false, 0, err
	}

	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return false, 0, errors.New("ratelimit: unexpected redis reply")
	}
	allowed, _ := arr[0].(int64)
	remaining, _ := arr[1].(int64)
	return allowed == 1, int(remaining), nil
}

// buildLimiterMember 生成 ZSet member。
// 形如 "{nowMs}-{seq}"，seq 由进程内 atomic.Uint64 单调递增产出，
// 保证同一进程并发请求互不去重；多进程间仍可能撞 member，
// 但概率极低（同毫秒同 seq 才会重合），且业务影响是最多多放行 1 次（fail-open 偏松），
// 不会出现「计数被吞」的偏紧问题。
func buildLimiterMember(nowMs int64) string {
	seq := memberSeq.Add(1)
	return strconv.FormatInt(nowMs, 10) + "-" + strconv.FormatUint(seq, 36)
}
