package hmac

import (
	"context"
	"testing"
)

// TestContextRoundTrip 注入后能取回同一个 appid。
func TestContextRoundTrip(t *testing.T) {
	ctx := NewContext(context.Background(), "svc-foo")
	got, ok := FromContext(ctx)
	if !ok || got != "svc-foo" {
		t.Fatalf("FromContext = (%q, %v), want (svc-foo, true)", got, ok)
	}
}

// TestFromContext_Empty 空 context 返回 ok=false。
func TestFromContext_Empty(t *testing.T) {
	if got, ok := FromContext(context.Background()); ok || got != "" {
		t.Fatalf("FromContext on empty ctx = (%q, %v), want (\"\", false)", got, ok)
	}
}
