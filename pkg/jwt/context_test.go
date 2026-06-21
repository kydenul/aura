package jwt

import (
	"context"
	"testing"
)

func TestContextRoundTrip(t *testing.T) {
	claims := &Claims{UserID: "u-1", Name: "alice", Roles: []string{"admin"}}
	ctx := NewContext(context.Background(), claims)

	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext 应返回 ok=true")
	}
	if got != claims {
		t.Fatalf("取出的 claims 与注入的不一致: got %+v", got)
	}
	if got.UserID != "u-1" || !got.HasRole("admin") {
		t.Errorf("claims 内容不正确: %+v", got)
	}
}

func TestFromContextMissing(t *testing.T) {
	got, ok := FromContext(context.Background())
	if ok {
		t.Error("空 context 应返回 ok=false")
	}
	if got != nil {
		t.Errorf("空 context 应返回 nil claims, got %+v", got)
	}
}
