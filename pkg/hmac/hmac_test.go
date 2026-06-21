package hmac

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testAppID  = "101"
	testSecret = "appkey-from-config"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(Config{
		Keys: map[string]string{testAppID: testSecret},
		Skew: 300 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// signedParams 构造一组带合法签名的参数，供成功 / 篡改用例复用。
func signedParams(now time.Time, body []byte) Params {
	p := Params{
		Method:    "POST",
		Path:      "/v1/users",
		AppID:     testAppID,
		Timestamp: strconv.FormatInt(now.Unix(), 10),
		Nonce:     "nonce-123",
		Body:      body,
	}
	p.Signature = Sign(testSecret, p)
	return p
}

// TestNewManager_NoKeys 无有效密钥时返回 ErrNoKeys。
func TestNewManager_NoKeys(t *testing.T) {
	if _, err := NewManager(Config{}); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("expect ErrNoKeys, got %v", err)
	}
	if _, err := NewManager(Config{Keys: map[string]string{"": "x", "k": ""}}); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("expect ErrNoKeys after filtering, got %v", err)
	}
}

// TestVerify_Success 合法签名应通过并返回 appid。
func TestVerify_Success(t *testing.T) {
	now := time.Now()
	appID, err := newTestManager(t).Verify(now, signedParams(now, []byte(`{"hello":"world"}`)))
	if err != nil {
		t.Fatalf("expect verify ok, got %v", err)
	}
	if appID != testAppID {
		t.Fatalf("expect appid %q, got %q", testAppID, appID)
	}
}

// TestSign_UppercaseHexAndCanonical 验证签名为大写 hex，且规范串首行为方案标识、含 query 与 body hash。
func TestSign_UppercaseHexAndCanonical(t *testing.T) {
	p := Params{
		Method: "POST", Path: "/v1/users", RawQuery: "b=2&a=1",
		AppID: testAppID, Timestamp: "1735689600", Nonce: "n1", Body: []byte("x"),
	}

	canonical := CanonicalRequest(p)
	lines := strings.Split(canonical, "\n")
	if len(lines) != 7 {
		t.Fatalf("canonical should have 7 lines, got %d: %q", len(lines), canonical)
	}
	if lines[0] != Scheme {
		t.Errorf("line0 = %q, want scheme %q", lines[0], Scheme)
	}
	// query 应按 key 字典序：a=1&b=2
	if lines[3] != "a=1&b=2" {
		t.Errorf("canonical query = %q, want a=1&b=2", lines[3])
	}
	// body hash = sha256("x") 小写 hex
	if lines[4] != hexSHA256([]byte("x")) {
		t.Errorf("body hash mismatch: %q", lines[4])
	}

	sig := Sign(testSecret, p)
	if sig != strings.ToUpper(sig) {
		t.Errorf("signature must be uppercase hex, got %q", sig)
	}
}

// TestCanonicalQuery_EmptyAndSorted 空 query 为空串；多参数按 key 升序。
func TestCanonicalQuery_EmptyAndSorted(t *testing.T) {
	if got := CanonicalQuery(""); got != "" {
		t.Errorf("empty query should be empty string, got %q", got)
	}
	if got := CanonicalQuery("z=1&a=2&m=3"); got != "a=2&m=3&z=1" {
		t.Errorf("CanonicalQuery sorted = %q, want a=2&m=3&z=1", got)
	}
}

// TestCanonicalQuery_MultiValueAllSigned 同名 key 的多值必须全部参与规范串，
// 否则攻击者可在保持签名不变的前提下追加额外值（隐形通道）。
func TestCanonicalQuery_MultiValueAllSigned(t *testing.T) {
	// 多值按值字典序拼接：a=1&a=2&a=3
	if got := CanonicalQuery("a=3&a=1&a=2"); got != "a=1&a=2&a=3" {
		t.Errorf("multi-value canonical = %q, want a=1&a=2&a=3", got)
	}
	// 与「只签首值」必须产生不同的规范串。
	if got := CanonicalQuery("a=1&a=2"); got == CanonicalQuery("a=1") {
		t.Errorf("multi-value query must differ from single value, got %q", got)
	}
}

// TestParseSignatureHeader 解析 X-Auth-Sign 头取 credential / signature。
func TestParseSignatureHeader(t *testing.T) {
	cred, sig := ParseSignatureHeader("TC-HMAC-SHA256 Credential=101,Signature=ABCDEF")
	if cred != "101" || sig != "ABCDEF" {
		t.Fatalf("ParseSignatureHeader = (%q,%q), want (101,ABCDEF)", cred, sig)
	}
	if c, s := ParseSignatureHeader("Bearer xxx"); c != "" || s != "" {
		t.Fatalf("non-scheme header should yield empty, got (%q,%q)", c, s)
	}
}

// TestVerify_MissingHeaders 缺少必需头或时间戳非法时返回 ErrMissingSignature。
func TestVerify_MissingHeaders(t *testing.T) {
	now := time.Now()
	m := newTestManager(t)
	cases := map[string]Params{
		"no appid":     {Timestamp: "1", Nonce: "n", Signature: "x"},
		"no timestamp": {AppID: testAppID, Nonce: "n", Signature: "x"},
		"no nonce":     {AppID: testAppID, Timestamp: "1", Signature: "x"},
		"no signature": {AppID: testAppID, Timestamp: "1", Nonce: "n"},
		"bad ts":       {AppID: testAppID, Timestamp: "nan", Nonce: "n", Signature: "x"},
	}
	for name, p := range cases {
		if _, err := m.Verify(now, p); !errors.Is(err, ErrMissingSignature) {
			t.Errorf("%s: expect ErrMissingSignature, got %v", name, err)
		}
	}
}

// TestVerify_UnknownAppID appid 未登记时返回 ErrUnknownAppID。
func TestVerify_UnknownAppID(t *testing.T) {
	now := time.Now()
	p := signedParams(now, nil)
	p.AppID = "999"
	p.Signature = Sign("whatever", p)
	if _, err := newTestManager(t).Verify(now, p); !errors.Is(err, ErrUnknownAppID) {
		t.Fatalf("expect ErrUnknownAppID, got %v", err)
	}
}

// TestVerify_TimestampExpired 时间戳越窗时返回 ErrTimestampExpired。
func TestVerify_TimestampExpired(t *testing.T) {
	now := time.Now()
	m, err := NewManager(Config{Keys: map[string]string{testAppID: testSecret}, Skew: 60 * time.Second})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	stale := now.Add(-10 * time.Minute)
	if _, err := m.Verify(now, signedParams(stale, nil)); !errors.Is(err, ErrTimestampExpired) {
		t.Fatalf("expect ErrTimestampExpired, got %v", err)
	}
}

// TestVerify_BodyTampered body 被篡改后签名不再匹配。
func TestVerify_BodyTampered(t *testing.T) {
	now := time.Now()
	p := signedParams(now, []byte(`{"amount":1}`))
	p.Body = []byte(`{"amount":1000000}`)
	if _, err := newTestManager(t).Verify(now, p); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expect ErrInvalidSignature, got %v", err)
	}
}
