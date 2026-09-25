package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeResolver struct {
	calls int
	token string
	ctx   context.Context
	id    string
	ok    bool
	err   error
}

func (f *fakeResolver) ResolveSession(ctx context.Context, token string) (string, bool, error) {
	f.calls++
	f.token, f.ctx = token, ctx
	return f.id, f.ok, f.err
}

func signedCookie(token string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(token))
	return token + "." + hex.EncodeToString(mac.Sum(nil))
}

func TestLoadSteamId(t *testing.T) {
	secret := []byte("test-secret")
	token := strings.Repeat("ab", 32)
	valid := signedCookie(token, secret)
	for _, tc := range []struct {
		name      string
		cookie    string
		secret    []byte
		resolved  bool
		err       error
		wantCalls int
		wantID    bool
	}{
		{name: "no cookie", secret: secret},
		{name: "no separator", cookie: token, secret: secret},
		{name: "invalid hex", cookie: token + ".zz", secret: secret},
		{name: "extra separator", cookie: valid + ".00", secret: secret},
		{name: "empty MAC", cookie: token + ".", secret: secret},
		{name: "empty token", cookie: signedCookie("", secret), secret: secret},
		{name: "forged MAC", cookie: token + "." + strings.Repeat("00", 32), secret: secret, resolved: true},
		{name: "tampered token", cookie: "cd" + valid[2:], secret: secret, resolved: true},
		{name: "wrong key", cookie: signedCookie(token, []byte("wrong-secret")), secret: secret, resolved: true},
		{name: "empty secret", cookie: signedCookie(token, nil), resolved: true},
		{name: "valid", cookie: valid, secret: secret, resolved: true, wantCalls: 1, wantID: true},
		{name: "unresolved", cookie: valid, secret: secret, wantCalls: 1},
		{name: "database error", cookie: valid, secret: secret, resolved: true, err: errors.New("unavailable"), wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const verifiedID = "76561198000000001"
			resolver := &fakeResolver{id: verifiedID, ok: tc.resolved, err: tc.err}
			auth := &Authenticator{Resolver: resolver, Secret: tc.secret}
			ctx := &CustomContext{Context: context.Background(), StartTime: time.Now()}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			// A legacy identity cookie must never authenticate the request.
			r.AddCookie(&http.Cookie{Name: "steam_id", Value: "client-controlled"})
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "session", Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			var load CustomMiddleware = auth.LoadSteamId
			if err := load(ctx, w, r); err != nil {
				t.Fatal(err)
			}
			if resolver.calls != tc.wantCalls {
				t.Fatalf("resolver calls = %d, want %d", resolver.calls, tc.wantCalls)
			}
			if resolver.calls != 0 && (resolver.token != token || resolver.ctx != r.Context()) {
				t.Fatal("resolver did not receive the token and request context")
			}
			identity := ctx.Value(SteamID{})
			cookies := w.Result().Cookies()
			if !tc.wantID {
				if identity != nil || len(cookies) != 0 {
					t.Fatal("unauthenticated request received an identity or refreshed cookie")
				}
				return
			}
			if identity != verifiedID {
				t.Fatal("context must hold the resolved SteamID64, not the cookie")
			}
			if len(cookies) != 1 {
				t.Fatalf("refreshed cookie count = %d", len(cookies))
			}
			cookie := cookies[0]
			if cookie.Name != "session" || cookie.Value != valid || cookie.Path != "/" || cookie.MaxAge != 7*24*3600 || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatal("refresh must preserve the signed value and set a secure seven-day cookie")
			}
		})
	}
}
