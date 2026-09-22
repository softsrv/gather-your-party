package auth

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// CLM-9: opaque token is 32 bytes from crypto/rand; two calls differ.
func TestNewSessionToken_32BytesAndUnique(t *testing.T) {
	a, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken err = %v", err)
	}
	b, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken err = %v", err)
	}
	// URL-safe base64 without padding of 32 bytes → 43 chars.
	if a == b {
		t.Fatalf("two NewSessionToken() calls returned identical values")
	}
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil {
		t.Fatalf("token not RawURLEncoded base64: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("decoded token length = %d, want 32", len(raw))
	}
}

// CLM-10: sign→verify round-trips the same token.
func TestSignVerify_RoundTrip(t *testing.T) {
	tok := "opaque-token-abc"
	secret := "supersecret"
	signed := SignToken(tok, secret)
	got, ok := VerifyToken(signed, secret)
	if !ok {
		t.Fatalf("VerifyToken ok = false, want true")
	}
	if got != tok {
		t.Fatalf("VerifyToken token = %q, want %q", got, tok)
	}
}

// CLM-10: altered token fails.
func TestVerify_AlteredTokenFails(t *testing.T) {
	tok := "opaque-token-abc"
	secret := "supersecret"
	signed := SignToken(tok, secret)
	i := strings.LastIndex(signed, "|")
	if i < 0 {
		t.Fatal("signed value has no delimiter")
	}
	// Flip a byte in the token portion.
	tampered := "x" + signed[1:]
	if tampered == signed {
		t.Fatal("tampering did not change the value")
	}
	if _, ok := VerifyToken(tampered, secret); ok {
		t.Fatalf("VerifyToken ok = true on altered token, want false")
	}
}

// CLM-10: altered MAC fails.
func TestVerify_AlteredMACFails(t *testing.T) {
	tok := "opaque-token-abc"
	secret := "supersecret"
	signed := SignToken(tok, secret)
	i := strings.LastIndex(signed, "|")
	if i < 0 {
		t.Fatal("signed value has no delimiter")
	}
	// Flip the last hex nibble of the MAC.
	rune0 := signed[len(signed)-1]
	var replacement byte = '0'
	if rune0 == '0' {
		replacement = '1'
	}
	tampered := signed[:len(signed)-1] + string(replacement)
	if _, ok := VerifyToken(tampered, secret); ok {
		t.Fatalf("VerifyToken ok = true on altered MAC, want false")
	}
}

// CLM-10: verify under a different secret fails.
func TestVerify_WrongSecretFails(t *testing.T) {
	signed := SignToken("t", "secret-a")
	if _, ok := VerifyToken(signed, "secret-b"); ok {
		t.Fatalf("VerifyToken ok = true under wrong secret, want false")
	}
}

// CLM-10: malformed cookie value fails.
func TestVerify_Malformed(t *testing.T) {
	for _, bad := range []string{"", "no-delimiter", "|nothexafter", "tok|nothex!"} {
		if _, ok := VerifyToken(bad, "s"); ok {
			t.Fatalf("VerifyToken ok = true on malformed %q, want false", bad)
		}
	}
}

// CLM-11: session cookie has HttpOnly, Secure, SameSite=Lax, MaxAge=30 days.
func TestNewSessionCookie_Attributes(t *testing.T) {
	c := NewSessionCookie("abc|def")
	if !c.HttpOnly {
		t.Errorf("HttpOnly = false, want true")
	}
	if !c.Secure {
		t.Errorf("Secure = false, want true")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	want := 30 * 24 * 60 * 60
	if c.MaxAge != want {
		t.Errorf("MaxAge = %d, want %d (30 days)", c.MaxAge, want)
	}
	if c.Name != SessionCookieName {
		t.Errorf("Name = %q, want %q", c.Name, SessionCookieName)
	}
}

// CLM-16: clearing cookie has negative MaxAge.
func TestClearSessionCookie(t *testing.T) {
	c := ClearSessionCookie()
	if c.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want < 0", c.MaxAge)
	}
	if c.Name != SessionCookieName {
		t.Errorf("Name = %q, want %q", c.Name, SessionCookieName)
	}
}
