package jwt

import (
	"errors"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

func newTestManager(t *testing.T, ttl time.Duration) *Manager {
	t.Helper()
	m, err := NewManager(Config{Secret: "test-secret", Issuer: "aura-test", TTL: ttl})
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}
	return m
}

func TestNewManagerEmptySecret(t *testing.T) {
	if _, err := NewManager(Config{}); !errors.Is(err, ErrEmptySecret) {
		t.Fatalf("want ErrEmptySecret, got %v", err)
	}
}

func TestGenerateAndParse(t *testing.T) {
	m := newTestManager(t, time.Hour)

	token, err := m.Generate("u-1", "alice", "alice@example.com", "admin")
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	claims, err := m.Parse(token)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if claims.UserID != "u-1" || claims.Name != "alice" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if !claims.HasRole("admin") || claims.HasRole("root") {
		t.Fatalf("HasRole mismatch: %+v", claims.Roles)
	}
}

func TestParseExpired(t *testing.T) {
	const secret = "test-secret"
	m, err := NewManager(Config{Secret: secret, Issuer: "aura-test", TTL: time.Hour})
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}

	// 直接用底层库签发一个已过期的 token（exp 在过去），绕过 Manager 对 TTL 的钳制。
	past := time.Now().Add(-time.Hour)
	claims := &Claims{
		UserID: "u-1",
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    "aura-test",
			IssuedAt:  jwtv5.NewNumericDate(past.Add(-time.Hour)),
			NotBefore: jwtv5.NewNumericDate(past.Add(-time.Hour)),
			ExpiresAt: jwtv5.NewNumericDate(past),
		},
	}
	token, err := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign expired token error: %v", err)
	}

	if _, err := m.Parse(token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

func TestParseWrongSecret(t *testing.T) {
	signer := newTestManager(t, time.Hour)
	token, err := signer.Generate("u-1", "", "")
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	other, _ := NewManager(Config{Secret: "another-secret", Issuer: "aura-test"})
	if _, err := other.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}
