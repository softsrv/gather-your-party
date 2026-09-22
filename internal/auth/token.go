// Package auth provides session token generation, HMAC signing/verification,
// and session-cookie construction for the signed-session auth flow that
// replaces the plaintext steam_id cookie path.
//
// Nothing in this package logs the SESSION_SECRET or the raw token value
// (CLM-12 MUST-forbid). Callers should keep that invariant when using
// these helpers.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
)

// SessionCookieName is the name of the signed session cookie set by the
// callback and read by the resolve-session middleware.
const SessionCookieName = "session"

// SessionMaxAgeSeconds mirrors the sessions row expiry (30 days). Kept in
// sync with store.sessionExpiry (30 * 24 * time.Hour). CLM-11.
const SessionMaxAgeSeconds = 30 * 24 * 60 * 60

// tokenLenBytes is the size of the opaque session token read from
// crypto/rand. 32 bytes = 256 bits of entropy. CLM-9.
const tokenLenBytes = 32

// NewSessionToken returns a URL-safe base64-encoded opaque session token
// backed by 32 bytes read from crypto/rand. Two calls will differ with
// overwhelming probability. CLM-9.
func NewSessionToken() (string, error) {
	var buf [tokenLenBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	// base64 URL encoding keeps the value safe as a cookie value.
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// SignToken returns a cookie value that carries the opaque token together
// with an HMAC-SHA256 of the token keyed by secret. The format is
// "<token>|<hexmac>". VerifyToken is the inverse. CLM-10.
func SignToken(token, secret string) string {
	mac := computeMAC(token, secret)
	return token + "|" + hex.EncodeToString(mac)
}

// VerifyToken parses a cookie value produced by SignToken and returns the
// contained token if the HMAC matches (constant-time compare via hmac.Equal).
// A value whose token or MAC has been altered returns ok=false. CLM-10, CLM-13.
func VerifyToken(cookieValue, secret string) (token string, ok bool) {
	i := strings.LastIndex(cookieValue, "|")
	if i <= 0 || i == len(cookieValue)-1 {
		return "", false
	}
	tok := cookieValue[:i]
	macHex := cookieValue[i+1:]
	gotMAC, err := hex.DecodeString(macHex)
	if err != nil {
		return "", false
	}
	wantMAC := computeMAC(tok, secret)
	if !hmac.Equal(gotMAC, wantMAC) {
		return "", false
	}
	return tok, true
}

func computeMAC(token, secret string) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(token))
	return h.Sum(nil)
}

// NewSessionCookie constructs the session cookie to set on the response.
// The cookie value is the signed (token|MAC) payload; the attributes are
// HttpOnly, Secure, SameSite=Lax, MaxAge=30 days. CLM-11.
func NewSessionCookie(signedValue string) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    signedValue,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   SessionMaxAgeSeconds,
	}
}

// ClearSessionCookie returns a cookie that instructs the browser to drop
// the session cookie (MaxAge < 0). Used by logout. CLM-16.
func ClearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}
