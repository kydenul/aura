package interceptor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	ahmac "aura/pkg/hmac"
)

const (
	mwAppID  = "101"
	mwSecret = "mw-secret"
)

func newMWManager(t *testing.T) *ahmac.Manager {
	t.Helper()
	m, err := ahmac.NewManager(ahmac.Config{
		Keys: map[string]string{mwAppID: mwSecret},
		Skew: 300 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// memNonceStore 内存版 NonceStore，用于断言去重逻辑。
type memNonceStore struct{ seen map[string]bool }

func newMemNonceStore() *memNonceStore { return &memNonceStore{seen: map[string]bool{}} }

func (s *memNonceStore) FirstSeen(_ context.Context, appID, nonce string, _ time.Duration) (bool, error) {
	key := appID + "_" + nonce
	if s.seen[key] {
		return false, nil
	}
	s.seen[key] = true
	return true, nil
}

// signRequest 按 TC-HMAC-SHA256 给请求加上合法签名头。
func signRequest(r *http.Request, body []byte, nonce string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := ahmac.Sign(mwSecret, ahmac.Params{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		AppID: mwAppID, Timestamp: ts, Nonce: nonce, Body: body,
	})
	r.Header.Set(ahmac.HeaderAppID, mwAppID)
	r.Header.Set(ahmac.HeaderTimestamp, ts)
	r.Header.Set(ahmac.HeaderNonce, nonce)
	r.Header.Set(ahmac.HeaderSignature,
		ahmac.Scheme+" Credential="+mwAppID+",Signature="+sig)
}

// TestHMACMiddleware_Success 合法签名应放行，并把 appid 注入 ctx。
func TestHMACMiddleware_Success(t *testing.T) {
	var gotAppID string
	h := HMACAuthMiddlewareWith(newMWManager(t), newMemNonceStore(), nil,
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			gotAppID, _ = ahmac.FromContext(r.Context())
		}))

	body := []byte(`{"k":"v"}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(string(body)))
	signRequest(r, body, "nonce-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	if gotAppID != mwAppID {
		t.Fatalf("expect appid %q injected, got %q", mwAppID, gotAppID)
	}
}

// TestHMACMiddleware_MissingHeaders 无签名头时返回 401。
func TestHMACMiddleware_MissingHeaders(t *testing.T) {
	h := HMACAuthMiddlewareWith(newMWManager(t), newMemNonceStore(), nil,
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatal("should not reach inner handler")
		}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/users", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expect 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHMACMiddleware_TamperedBody 头按原始 body 签名、实际 body 被篡改时拒绝。
func TestHMACMiddleware_TamperedBody(t *testing.T) {
	h := HMACAuthMiddlewareWith(newMWManager(t), newMemNonceStore(), nil,
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatal("should not reach inner handler")
		}))

	const path = "/v1/users"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := ahmac.Sign(mwSecret, ahmac.Params{
		Method: http.MethodPost, Path: path, AppID: mwAppID,
		Timestamp: ts, Nonce: "n-1", Body: []byte(`{"amount":1}`),
	})

	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"amount":999}`))
	r.Header.Set(ahmac.HeaderAppID, mwAppID)
	r.Header.Set(ahmac.HeaderTimestamp, ts)
	r.Header.Set(ahmac.HeaderNonce, "n-1")
	r.Header.Set(ahmac.HeaderSignature,
		ahmac.Scheme+" Credential="+mwAppID+",Signature="+sig)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expect 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHMACMiddleware_NonceReplay 同一 nonce 第二次请求被去重拒绝。
func TestHMACMiddleware_NonceReplay(t *testing.T) {
	store := newMemNonceStore()
	mgr := newMWManager(t)
	var hits int
	h := HMACAuthMiddlewareWith(mgr, store, nil,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.WriteHeader(http.StatusOK)
		}))

	body := []byte(`{"k":"v"}`)
	// 第一次：通过
	r1 := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(string(body)))
	signRequest(r1, body, "dup-nonce")
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first request expect 200, got %d", w1.Code)
	}

	// 第二次：同 nonce，重放被拒
	r2 := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(string(body)))
	signRequest(r2, body, "dup-nonce")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("replayed request expect 401, got %d", w2.Code)
	}
	if hits != 1 {
		t.Fatalf("inner handler should run once, got %d", hits)
	}
}

// TestHMACMiddleware_NilNonceStore nonces 为 nil 时跳过去重，仅靠时间窗，仍放行合法请求。
func TestHMACMiddleware_NilNonceStore(t *testing.T) {
	var reached bool
	h := HMACAuthMiddlewareWith(newMWManager(t), nil, nil,
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { reached = true }))

	body := []byte(`{"k":"v"}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(string(body)))
	signRequest(r, body, "n-x")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !reached {
		t.Fatal("legal request should pass when nonce store is nil")
	}
}

// TestHMACMiddleware_PrefixGating 配置了 protectedPrefixes 时，仅命中前缀的路由验签，其余直接放行。
func TestHMACMiddleware_PrefixGating(t *testing.T) {
	prefixes := []string{"/v1/openapi/"}

	// 未命中前缀：无签名也直接放行。
	var passed bool
	h := HMACAuthMiddlewareWith(newMWManager(t), newMemNonceStore(), prefixes,
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { passed = true }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/users", nil))
	if !passed || w.Code == http.StatusUnauthorized {
		t.Fatalf("unprotected path should pass without signature, code=%d passed=%v", w.Code, passed)
	}

	// 命中前缀：无签名被拒。
	h2 := HMACAuthMiddlewareWith(newMWManager(t), newMemNonceStore(), prefixes,
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatal("protected path without signature should not reach handler")
		}))
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/v1/openapi/things", nil))
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("protected path expect 401, got %d", w2.Code)
	}
}

// TestHMACMiddleware_NonceTTLCoversBoundary 中间件传给 NonceStore 的 TTL 必须 = 2×Skew，
// 覆盖 Verify 时间窗的正负向偏移并留出过期精度余量；否则边界缝隙可被重放。
func TestHMACMiddleware_NonceTTLCoversBoundary(t *testing.T) {
	mgr, err := ahmac.NewManager(ahmac.Config{
		Keys: map[string]string{mwAppID: mwSecret},
		Skew: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	spy := &ttlSpyNonceStore{}
	h := HMACAuthMiddlewareWith(mgr, spy, nil,
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

	body := []byte(`{"k":"v"}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(string(body)))
	signRequest(r, body, "n-ttl")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if want := 2 * mgr.Skew(); spy.lastTTL != want {
		t.Fatalf("FirstSeen ttl = %v, want %v (= 2×Skew)", spy.lastTTL, want)
	}
}

// ttlSpyNonceStore 仅捕获最近一次 FirstSeen 收到的 ttl 参数。
type ttlSpyNonceStore struct{ lastTTL time.Duration }

func (s *ttlSpyNonceStore) FirstSeen(_ context.Context, _, _ string, ttl time.Duration) (bool, error) {
	s.lastTTL = ttl
	return true, nil
}

// TestHMACMiddleware_BodyRestored 验签后下游仍能读到完整 body。
func TestHMACMiddleware_BodyRestored(t *testing.T) {
	body := []byte(`{"k":"v","n":123}`)
	var seen string
	h := HMACAuthMiddlewareWith(newMWManager(t), newMemNonceStore(), nil,
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			seen = string(b)
		}))

	r := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(string(body)))
	signRequest(r, body, "n-restore")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if seen != string(body) {
		t.Fatalf("downstream body = %q, want %q", seen, string(body))
	}
}
