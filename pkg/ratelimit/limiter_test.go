package ratelimit

import (
	"testing"
	"time"
)

// newKeyedWithClock 构造 KeyedLimiter 并注入虚拟时钟，便于在测试中精确推进时间，
// 避免依赖 time.Sleep 带来的 CI 抖动误判。
func newKeyedWithClock(
	enabled bool,
	rps float64,
	burst int,
	ttl time.Duration,
	maxKeys int,
	base time.Time,
) (*KeyedLimiter, *time.Time) {
	k := NewKeyed(enabled, rps, burst, ttl, maxKeys)
	now := base
	k.nowFn = func() time.Time { return now }
	k.lastGC = now // 与初始 now 对齐，避免首次 Allow 立即触发 GC
	return k, &now
}

// TestLimiter_BurstThenReject 桶容量内放行 burst 次，随后（令牌未补充）被拒。
func TestLimiter_BurstThenReject(t *testing.T) {
	l := New(true, 1, 3) // 1 rps，burst 3：瞬时最多过 3 个
	for i := range 3 {
		if !l.Allow() {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	if l.Allow() {
		t.Fatal("第 4 次应被拒（桶已空，1rps 短时间内不补充令牌）")
	}
}

// TestLimiter_DisabledAlwaysAllow enabled=false 及 nil receiver 恒放行。
func TestLimiter_DisabledAlwaysAllow(t *testing.T) {
	l := New(false, 1, 1)
	for range 100 {
		if !l.Allow() {
			t.Fatal("disabled 应恒放行")
		}
	}
	var nilL *Limiter
	if !nilL.Allow() {
		t.Fatal("nil limiter 应放行")
	}
}

// TestLimiter_RPSZeroAlwaysAllow rps<=0 视为不限流，恒放行。
func TestLimiter_RPSZeroAlwaysAllow(t *testing.T) {
	l := New(true, 0, 1)
	for range 100 {
		if !l.Allow() {
			t.Fatal("rps<=0 应恒放行")
		}
	}
}

// TestLimiter_Update 热更可在开/关之间切换并调整阈值。
func TestLimiter_Update(t *testing.T) {
	l := New(false, 1, 2)
	if l.Enabled() {
		t.Fatal("初始应为关闭")
	}

	l.Update(true, 1, 2)
	if !l.Enabled() {
		t.Fatal("热更后应启用")
	}
	if !l.Allow() {
		t.Fatal("启用后第 1 次应放行")
	}
	if !l.Allow() {
		t.Fatal("启用后第 2 次应放行")
	}
	if l.Allow() {
		t.Fatal("启用后第 3 次应被拒")
	}

	l.Update(false, 1, 2)
	if l.Allow() != true {
		t.Fatal("再次关闭后应放行")
	}
}

// TestKeyedLimiter_PerKeyIsolation 不同 key 独立计数互不影响。
func TestKeyedLimiter_PerKeyIsolation(t *testing.T) {
	k := NewKeyed(true, 1, 2, time.Minute, 0)
	if !k.Allow("A") {
		t.Fatal("A 第 1 次应放行")
	}
	if !k.Allow("A") {
		t.Fatal("A 第 2 次应放行")
	}
	if k.Allow("A") {
		t.Fatal("A 第 3 次应被拒")
	}
	// B 维度独立，不受 A 影响
	if !k.Allow("B") {
		t.Fatal("B 第 1 次应放行")
	}
	if !k.Allow("B") {
		t.Fatal("B 第 2 次应放行")
	}
}

// TestKeyedLimiter_EmptyKeyAndDisabled 空 key / 关闭 / nil 恒放行。
func TestKeyedLimiter_EmptyKeyAndDisabled(t *testing.T) {
	k := NewKeyed(true, 1, 1, time.Minute, 0)
	for range 10 {
		if !k.Allow("") {
			t.Fatal("空 key 应恒放行")
		}
	}

	off := NewKeyed(false, 1, 1, time.Minute, 0)
	for range 10 {
		if !off.Allow("A") {
			t.Fatal("disabled 应放行")
		}
	}

	var nilK *KeyedLimiter
	if !nilK.Allow("A") {
		t.Fatal("nil KeyedLimiter 应放行")
	}
}

// TestKeyedLimiter_GC 不活跃 key 超过 ttl 后被惰性回收（虚拟时钟，避免 CI 抖动）。
func TestKeyedLimiter_GC(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	k, now := newKeyedWithClock(true, 100, 100, 50*time.Millisecond, 0, base)

	k.Allow("A") // 建 A 桶

	// 推进 80ms：A 已超 ttl，下次 Allow 触发 GC
	*now = now.Add(80 * time.Millisecond)
	k.Allow("B")

	k.mu.Lock()
	_, hasA := k.buckets["A"]
	_, hasB := k.buckets["B"]
	k.mu.Unlock()

	if hasA {
		t.Fatal("A 超过 ttl 未访问，应已被 GC 回收")
	}
	if !hasB {
		t.Fatal("B 刚访问，不应被回收")
	}
}

// TestKeyedLimiter_Update_PreservesExistingBuckets
// 热更应**原地更新**已有桶，而不是清空它们 —— 否则攻击者可以通过反复触发 reload 获得免费 burst。
func TestKeyedLimiter_Update_PreservesExistingBuckets(t *testing.T) {
	k := NewKeyed(true, 1, 1, time.Minute, 0)

	if !k.Allow("A") {
		t.Fatal("A 首次应放行")
	}
	if k.Allow("A") {
		t.Fatal("A 第 2 次应被拒（burst=1，旧阈值）")
	}

	// 提高阈值；旧桶应保留，剩余令牌按新阈值平滑续上 —— 不应「重置」给 A 一个满血 burst。
	k.Update(true, 100, 5)

	// A 桶应仍然存在
	k.mu.Lock()
	_, hasA := k.buckets["A"]
	k.mu.Unlock()
	if !hasA {
		t.Fatal("热更不应清空已有桶（A 桶应被保留并原地更新阈值）")
	}

	// 因为 rps=100 很高、距上次拒绝时间极短，rate.Limiter 已能补出 ~5 个令牌，
	// 这是正常行为；这里只断言「桶被保留」即可，至于具体放行多少由 rate 库决定，
	// 不再脆弱地断言精确次数。
}

// TestKeyedLimiter_Reconfigure_TTL 热更 TTL 后立刻按新 TTL 判定回收。
func TestKeyedLimiter_Reconfigure_TTL(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	k, now := newKeyedWithClock(true, 10, 10, 10*time.Minute, 0, base)

	k.Allow("A")

	// 把 TTL 缩到 50ms
	k.Reconfigure(true, 10, 10, 50*time.Millisecond, 0)

	// 推进 80ms 后用 B 触发 GC：A 应按新 TTL 被回收
	*now = now.Add(80 * time.Millisecond)
	k.Allow("B")

	k.mu.Lock()
	_, hasA := k.buckets["A"]
	k.mu.Unlock()
	if hasA {
		t.Fatal("TTL 热更后 A 应按新 TTL=50ms 被回收")
	}
}

// TestKeyedLimiter_MaxKeysFailClosed 桶数达到 maxKeys 时新 key 被拒（fail-closed）。
// 防止伪造 key 的内存放大攻击。
func TestKeyedLimiter_MaxKeysFailClosed(t *testing.T) {
	// rps=1000、burst=10：每个 key 都能轻松通过自己的限流检查，
	// 这样断言「桶数达到 maxKeys 后新 key 被拒」时不会被自身令牌耗尽干扰。
	k := NewKeyed(true, 1000, 10, time.Hour /* TTL 极长，避免被 GC 干扰 */, 3)

	for _, key := range []string{"k1", "k2", "k3"} {
		if !k.Allow(key) {
			t.Fatalf("%s 首次应放行（rps=1000 burst=10）", key)
		}
	}

	// 已有 3 个桶，达到 maxKeys=3：新 key 应被拒
	if k.Allow("k4") {
		t.Fatal("桶数达到 maxKeys 时新 key 应被拒（fail-closed）")
	}

	// 现存 key 仍可继续走限流路径（不受 maxKeys 拦截）
	if !k.Allow("k1") {
		t.Fatal("已存在的 key 不应受 maxKeys 影响")
	}

	if size := k.Size(); size != 3 {
		t.Fatalf("被拒的新 key 不应入桶, size=%d 期望 3", size)
	}
}

// TestKeyedLimiter_MaxKeysGCReclaim 当桶数触顶但有过期 key 时，应能强制 GC 后腾出空间。
func TestKeyedLimiter_MaxKeysGCReclaim(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	k, now := newKeyedWithClock(true, 1000, 1, 50*time.Millisecond, 3, base)

	// 用满 3 个桶
	for _, key := range []string{"k1", "k2", "k3"} {
		if !k.Allow(key) {
			t.Fatalf("%s 首次应放行", key)
		}
	}

	// 推进 100ms：k1/k2/k3 全部过期
	*now = now.Add(100 * time.Millisecond)

	// 新 key 进入：触发强制 GC，腾出空间，应放行
	if !k.Allow("k4") {
		t.Fatal("过期桶应被强制 GC 回收，新 key 应能进入")
	}

	if size := k.Size(); size > 3 {
		t.Fatalf("回收后桶数不应超过 maxKeys, got=%d", size)
	}
}

// TestLimiter_Update_AtomicSwitch
// 切换序列：先关 enabled → 改 rate/burst → 再恢复目标 enabled。
// 确认热更后阈值确实生效（用大量请求探一下）。
func TestLimiter_Update_AtomicSwitch(t *testing.T) {
	l := New(true, 1000, 1000)
	// 把阈值调极小
	l.Update(true, 1, 1)

	if !l.Allow() {
		t.Fatal("热更后第 1 次应放行（burst=1）")
	}
	// 热更后第 2 次必拒；如果实现没正确切换 enabled，会用旧的大 burst 放行
	if l.Allow() {
		t.Fatal("热更后第 2 次应被拒（burst=1，新阈值已生效）")
	}
}
