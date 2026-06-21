// Package ratelimit 提供基于令牌桶（golang.org/x/time/rate）的「单机」限流器。
//
// 与 pkg/redis 的 SlidingWindowLimiter（分布式、跨实例全局一致）不同，
// 本包是纯进程内（单机）限流，不依赖任何外部存储：
//   - 零网络开销、零外部依赖，专门做「单实例过载保护」，防止瞬时洪峰把接口打崩；
//   - 多副本部署时各实例独立计数（N 副本 → 整体容量约为单实例阈值的 N 倍）；
//   - 进程重启计数清零。
//
// 底层使用 Go 官方扩展库 golang.org/x/time/rate 的令牌桶算法：
//   - rps   控制稳态平均速率（每秒产生的令牌数）；
//   - burst 控制瞬时突发上限（桶容量，可同时通过的最大请求数）。
//
// 两个限流维度：
//   - Limiter：      全局维度，整机共享一个桶 —— 防止总流量打崩下游（DB/Redis/协程）。
//   - KeyedLimiter： 按 key（如 client IP）维度，每个 key 独立一个桶 —— 防止单一来源
//     突发独占容量；长时间不活跃的 key 惰性回收，且总 key 数有上限以防内存放大攻击。
//
// 所有限流器均支持运行期热更（Update / Reconfigure），配合 config 热更回调可在不重启
// 服务的情况下调整阈值。本包零依赖业务 / config 包，由 main 在启动期用 config 的值装配
// （对齐 pkg/db、pkg/redis 等组件）。
package ratelimit

import (
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// defaultMaxKeys KeyedLimiter 默认最大 key 数；超过时拒绝新 key（fail-closed），
// 防止伪造 key 的 DoS 攻击把 buckets map 撑爆。10w 条按每条 ~80B 估算 ~8MB，可控。
const defaultMaxKeys = 100_000

// Limiter 单机令牌桶限流器（全局维度，整个进程共享一个桶）。
// 并发安全，可被多个 goroutine 同时调用。
type Limiter struct {
	// inner 始终非 nil；关闭限流时通过 enabled=false 短路，便于热更直接 SetLimit/SetBurst。
	inner *rate.Limiter
	// enabled 限流开关（原子读写）。false 时 Allow 恒放行。
	enabled atomic.Bool
}

// New 构造全局令牌桶限流器。
//   - enabled: 总开关；false 或 rps<=0 时 Allow 恒放行（不限流）。
//   - rps:     每秒允许的平均请求数（令牌产生速率）。
//   - burst:   突发桶容量（瞬时可同时通过的最大请求数）；<1 时自动取 1。
func New(enabled bool, rps float64, burst int) *Limiter {
	l := &Limiter{inner: rate.NewLimiter(limitOf(rps), burstOf(burst))}
	l.enabled.Store(enabled && rps > 0)
	return l
}

// Allow 上报一次请求，返回是否放行（放行时消耗一个令牌）。
// 限流关闭（enabled=false / rps<=0）或 receiver 为 nil 时恒放行。
func (l *Limiter) Allow() bool {
	if l == nil || !l.enabled.Load() {
		return true
	}
	return l.inner.Allow()
}

// Update 运行期热更阈值（并发安全，无需重启服务）。
//
// 切换序列：先关 enabled → 改 rate/burst → 再恢复目标 enabled。
// 这样在阈值切换的微窗口内会短暂「恒放行」，而不会出现「用旧 rps/burst 放行」的不一致状态；
// 对单机限流来说前者完全可接受（毫秒级窗口、过载保护语义本来就是兜底）。
//
// rate.Limiter.SetLimit/SetBurst 本身是并发安全的。
func (l *Limiter) Update(enabled bool, rps float64, burst int) {
	if l == nil {
		return
	}
	l.enabled.Store(false)
	l.inner.SetLimit(limitOf(rps))
	l.inner.SetBurst(burstOf(burst))
	l.enabled.Store(enabled && rps > 0)
}

// Enabled 返回当前限流是否生效（供日志 / 排障观测）。
func (l *Limiter) Enabled() bool {
	return l != nil && l.enabled.Load()
}

// keyedEntry 单个 key 的限流器及其最近访问时间（用于惰性 GC）。
type keyedEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// KeyedLimiter 按 key（如 client IP / user_id）维度的单机令牌桶限流器。
// 每个 key 独立一个令牌桶；长时间不活跃的 key 被惰性回收，并设有最大 key 数上限，
// 避免单连接伪造大量 key 撑爆内存。
//
// 适用场景：在全局限流之外，防止「单一来源」突发独占整机容量。
type KeyedLimiter struct {
	mu      sync.Mutex
	buckets map[string]*keyedEntry

	enabled bool
	rps     rate.Limit
	burst   int

	// ttl 超过该时长未访问的 key 会在下次 GC 时被回收。
	ttl time.Duration
	// lastGC 上次回收时间；惰性触发（无后台 goroutine），间隔 >= ttl 才扫描一次。
	lastGC time.Time

	// maxKeys 已存活 key 的硬上限；超过时新 key 被拒（fail-closed）。
	// 与过载保护语义一致：内存接近上限时，宁可错杀也不让攻击者把进程打崩。
	maxKeys int

	// nowFn 返回当前时间；测试时可替换为虚拟时钟，避免 time.Sleep。
	nowFn func() time.Time
}

// NewKeyed 构造按 key 维度的令牌桶限流器。
//   - enabled:  总开关；false 或 rps<=0 时 Allow 恒放行。
//   - rps:      单个 key 每秒允许的平均请求数。
//   - burst:    单个 key 的突发桶容量；<1 时自动取 1。
//   - ttl:      key 不活跃回收阈值；<=0 时取默认 10 分钟。
//   - maxKeys:  最大并发 key 数，达到上限后新 key 被拒；<=0 时取默认值。
func NewKeyed(enabled bool, rps float64, burst int, ttl time.Duration, maxKeys int) *KeyedLimiter {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if maxKeys <= 0 {
		maxKeys = defaultMaxKeys
	}
	now := time.Now()
	return &KeyedLimiter{
		buckets: make(map[string]*keyedEntry),
		enabled: enabled && rps > 0,
		rps:     limitOf(rps),
		burst:   burstOf(burst),
		ttl:     ttl,
		lastGC:  now,
		maxKeys: maxKeys,
		nowFn:   time.Now,
	}
}

// Allow 上报 key 的一次请求，返回是否放行。
//
// 行为：
//   - receiver 为 nil 或 key 为空 → 恒放行（无法归类，交由全局限流兜底）；
//   - 限流关闭 → 恒放行；
//   - 桶数达到 maxKeys 上限 → 先尝试 GC 一次回收过期桶；仍满则拒绝新 key（fail-closed）；
//   - 命中已有桶 → 调用 rate.Limiter.Allow 决定。
func (k *KeyedLimiter) Allow(key string) bool {
	if k == nil || key == "" {
		return true
	}

	k.mu.Lock()
	if !k.enabled {
		k.mu.Unlock()
		return true
	}

	now := k.nowFn()
	k.gcLocked(now)

	e, ok := k.buckets[key]
	if !ok {
		// 容量已满：强制 GC 一次（忽略 lastGC 间隔）后再判定。
		if len(k.buckets) >= k.maxKeys {
			k.forceGCLocked(now)
			if len(k.buckets) >= k.maxKeys {
				k.mu.Unlock()
				return false
			}
		}
		e = &keyedEntry{limiter: rate.NewLimiter(k.rps, k.burst)}
		k.buckets[key] = e
	}
	e.lastSeen = now
	lim := e.limiter
	k.mu.Unlock()

	return lim.Allow()
}

// Reconfigure 运行期热更阈值（并发安全）。
//
// 与早期实现「清空旧桶」不同：本实现对所有现存桶**原地**调用 SetLimit/SetBurst。
// 原因：清空会让正在被限流的 key 立即获得满血 burst —— 在持续攻击中调阈值反而给攻击者
// 一次「免费瞬时双倍配额」，且每次 yaml 抖动都会反复重置，等同绕过限流。原地更新可让
// 旧桶的剩余令牌按新阈值平滑过渡。
//
//   - ttl<=0     时保留当前 ttl；
//   - maxKeys<=0 时保留当前 maxKeys。
func (k *KeyedLimiter) Reconfigure(
	enabled bool,
	rps float64,
	burst int,
	ttl time.Duration,
	maxKeys int,
) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()

	k.enabled = enabled && rps > 0
	k.rps = limitOf(rps)
	k.burst = burstOf(burst)
	if ttl > 0 {
		k.ttl = ttl
	}
	if maxKeys > 0 {
		k.maxKeys = maxKeys
	}

	// 原地更新所有现存桶：旧桶的剩余令牌不重置，平滑切换到新 rps/burst。
	newBurst := burstOf(burst)
	for _, e := range k.buckets {
		e.limiter.SetLimit(k.rps)
		e.limiter.SetBurst(newBurst)
	}
}

// Update 是 Reconfigure 的简化形式，仅热更 enabled/rps/burst（保留当前 ttl/maxKeys）。
func (k *KeyedLimiter) Update(enabled bool, rps float64, burst int) {
	k.Reconfigure(enabled, rps, burst, 0, 0)
}

// Enabled 返回当前限流是否生效（供日志 / 排障观测）。
func (k *KeyedLimiter) Enabled() bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.enabled
}

// Size 返回当前存活的桶数（供观测 / 排障）。
func (k *KeyedLimiter) Size() int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.buckets)
}

// gcLocked 惰性回收过期 key（调用方须持有 k.mu）。
// 距上次 GC 未超过 ttl 时直接跳过，避免每次请求都全表扫描。
func (k *KeyedLimiter) gcLocked(now time.Time) {
	if now.Sub(k.lastGC) < k.ttl {
		return
	}
	k.forceGCLocked(now)
}

// forceGCLocked 立即扫描并回收过期桶，无视 lastGC 间隔（用于 maxKeys 触顶时的兜底回收）。
// 调用方须持有 k.mu。
func (k *KeyedLimiter) forceGCLocked(now time.Time) {
	for key, e := range k.buckets {
		if now.Sub(e.lastSeen) >= k.ttl {
			delete(k.buckets, key)
		}
	}
	k.lastGC = now
}

// limitOf 把 rps 转成 rate.Limit；rps<=0 视为「不限速」（rate.Inf）。
func limitOf(rps float64) rate.Limit {
	if rps <= 0 {
		return rate.Inf
	}
	return rate.Limit(rps)
}

// burstOf 归一化突发容量，至少为 1（rate.NewLimiter 的 burst<1 会拒绝所有请求）。
func burstOf(burst int) int {
	if burst < 1 {
		return 1
	}
	return burst
}
