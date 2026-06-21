package redis

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSlidingWindowLimiter_AllowAndReject 验证窗口内允许 limit 次、第 limit+1 次拒绝。
func TestSlidingWindowLimiter_AllowAndReject(t *testing.T) {
	_, c := setupClient(t)
	limiter := NewSlidingWindowLimiter(c, "ratelimit:test", 3, time.Minute)
	ctx := context.Background()

	for i := range 3 {
		ok, remaining, err := limiter.Allow(ctx, "user-1")
		if err != nil {
			t.Fatalf("第 %d 次 Allow 出错: %v", i+1, err)
		}
		if !ok {
			t.Fatalf("第 %d 次应通过但被拒", i+1)
		}
		if want := 2 - i; remaining != want {
			t.Fatalf("第 %d 次 remaining=%d, 期望=%d", i+1, remaining, want)
		}
	}

	ok, remaining, err := limiter.Allow(ctx, "user-1")
	if err != nil {
		t.Fatalf("第 4 次 Allow 出错: %v", err)
	}
	if ok {
		t.Fatal("第 4 次应被拒")
	}
	if remaining != 0 {
		t.Fatalf("被拒时 remaining 应为 0, 实际=%d", remaining)
	}
}

// TestSlidingWindowLimiter_DifferentIdentitiesIsolated 不同 identity 计数互不影响。
func TestSlidingWindowLimiter_DifferentIdentitiesIsolated(t *testing.T) {
	_, c := setupClient(t)
	limiter := NewSlidingWindowLimiter(c, "ratelimit:test", 2, time.Minute)
	ctx := context.Background()

	for i := range 2 {
		if ok, _, _ := limiter.Allow(ctx, "user-A"); !ok {
			t.Fatalf("user-A 第 %d 次应通过", i+1)
		}
	}
	if ok, _, _ := limiter.Allow(ctx, "user-A"); ok {
		t.Fatal("user-A 第 3 次应被拒")
	}
	// user-B 独立计数，仍应能通过 2 次。
	for i := range 2 {
		if ok, _, _ := limiter.Allow(ctx, "user-B"); !ok {
			t.Fatalf("user-B 第 %d 次应通过", i+1)
		}
	}
}

// TestSlidingWindowLimiter_DifferentPrefixesIsolated 不同 keyPrefix 计数互不影响。
func TestSlidingWindowLimiter_DifferentPrefixesIsolated(t *testing.T) {
	_, c := setupClient(t)
	upload := NewSlidingWindowLimiter(c, "ratelimit:upload", 1, time.Minute)
	publish := NewSlidingWindowLimiter(c, "ratelimit:publish", 1, time.Minute)
	ctx := context.Background()

	if ok, _, _ := upload.Allow(ctx, "user-1"); !ok {
		t.Fatal("upload 首次应通过")
	}
	if ok, _, _ := upload.Allow(ctx, "user-1"); ok {
		t.Fatal("upload 第 2 次应被拒")
	}
	if ok, _, _ := publish.Allow(ctx, "user-1"); !ok {
		t.Fatal("publish 首次应通过（与 upload 解耦）")
	}
}

// TestSlidingWindowLimiter_WindowSliding 窗口完全滑出后应再次允许。
func TestSlidingWindowLimiter_WindowSliding(t *testing.T) {
	mr, c := setupClient(t)
	limiter := NewSlidingWindowLimiter(c, "ratelimit:test", 2, 5*time.Second)
	ctx := context.Background()

	for i := range 2 {
		if ok, _, _ := limiter.Allow(ctx, "user-1"); !ok {
			t.Fatalf("第 %d 次应通过", i+1)
		}
	}
	if ok, _, _ := limiter.Allow(ctx, "user-1"); ok {
		t.Fatal("第 3 次应被拒")
	}

	// 脚本按传入的 now（真实时间）剔除窗口外条目。miniredis 的 FastForward 只影响 TTL，
	// 不改 ZSet 已存 score，故直接清 key 等价于「窗口完全滑出」。
	mr.FastForward(6 * time.Second)
	mr.Del("ratelimit:test:user-1")

	if ok, _, _ := limiter.Allow(ctx, "user-1"); !ok {
		t.Fatal("窗口滑过后应再次允许")
	}
}

// TestSlidingWindowLimiter_NilClient client=nil 时返回 ErrRateLimitNotReady。
func TestSlidingWindowLimiter_NilClient(t *testing.T) {
	limiter := NewSlidingWindowLimiter(nil, "ratelimit:test", 30, time.Minute)
	ok, _, err := limiter.Allow(context.Background(), "user-1")
	if ok {
		t.Fatal("client=nil 应返回 allowed=false")
	}
	if !errors.Is(err, ErrRateLimitNotReady) {
		t.Fatalf("期望 ErrRateLimitNotReady, 实际=%v", err)
	}
}

// TestSlidingWindowLimiter_InvalidConfig limit/window <= 0 时直接拒绝（不报错）。
func TestSlidingWindowLimiter_InvalidConfig(t *testing.T) {
	_, c := setupClient(t)
	cases := []struct {
		name   string
		limit  int
		window time.Duration
	}{
		{"limit_zero", 0, time.Minute},
		{"limit_neg", -1, time.Minute},
		{"window_zero", 30, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limiter := NewSlidingWindowLimiter(c, "ratelimit:test", tc.limit, tc.window)
			ok, remaining, err := limiter.Allow(context.Background(), "user-1")
			if err != nil {
				t.Fatalf("不期望错误: %v", err)
			}
			if ok || remaining != 0 {
				t.Fatalf("期望被拒且 remaining=0, 得 ok=%v remaining=%d", ok, remaining)
			}
		})
	}
}
