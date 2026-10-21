package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/template"
)

// Google OAuth endpoints. Package variables, like steamOpenIDEndpoint, so
// tests can point them at local servers.
var (
	googleAuthEndpoint   = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenEndpoint  = "https://oauth2.googleapis.com/token"
	googleRevokeEndpoint = "https://oauth2.googleapis.com/revoke"
)

// googleHTTPClient bounds every server-to-Google call.
var googleHTTPClient = &http.Client{Timeout: 10 * time.Second}

const (
	// googleCalendarScope is exactly the access the connection asks for.
	googleCalendarScope = "openid email https://www.googleapis.com/auth/calendar.readonly"
	// googleStateCookie carries the signed OAuth state between connect and callback.
	googleStateCookie = "google_calendar_state"
	// googleStateCookiePath covers the callback route (and the connect route).
	googleStateCookiePath = "/auth/google/calendar"
	googleStateMaxAge     = 10 * time.Minute

	// calendarQueryKey/calendarQueryError form the fixed failure flag on the
	// profile URL. The value is a constant, never taken from the request.
	calendarQueryKey   = "calendar"
	calendarQueryError = "error"
	calendarErrorPath  = "/profile?" + calendarQueryKey + "=" + calendarQueryError
)

// googleStateMAC binds a state value to the user who started the connect.
func (app *application) googleStateMAC(userID int64, state string) string {
	mac := hmac.New(sha256.New, []byte(app.config.SessionSecret))
	mac.Write([]byte("google-calendar-state|" + strconv.FormatInt(userID, 10) + "|" + state))
	return hex.EncodeToString(mac.Sum(nil))
}

func googleStateCookieFor(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     googleStateCookie,
		Value:    value,
		Path:     googleStateCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		// Lax, not Strict: the browser must send it on Google's top-level
		// redirect back to the callback.
		SameSite: http.SameSiteLaxMode,
	}
}

// handleGoogleCalendarConnect sends a signed-in user to Google's consent
// screen, or back to the profile when Google is not configured.
func (app *application) handleGoogleCalendarConnect(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	userID, ok := app.sessionUserID(ctx, w, r)
	if !ok {
		return
	}
	if !app.config.googleAvailable() || app.config.SessionSecret == "" {
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		fmt.Printf("google calendar state: %s\n", err)
		http.Redirect(w, r, calendarErrorPath, http.StatusSeeOther)
		return
	}
	state := hex.EncodeToString(raw)
	u, err := url.Parse(googleAuthEndpoint)
	if err != nil {
		http.Redirect(w, r, calendarErrorPath, http.StatusSeeOther)
		return
	}
	u.RawQuery = url.Values{
		"client_id":     {app.config.GoogleClientID},
		"redirect_uri":  {app.googleRedirectURI()},
		"response_type": {"code"},
		"scope":         {googleCalendarScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}.Encode()
	http.SetCookie(w, googleStateCookieFor(state+"."+app.googleStateMAC(userID, state), int(googleStateMaxAge.Seconds())))
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// googleRedirectURI is built from the configured origin, never request headers.
func (app *application) googleRedirectURI() string {
	return app.config.AppBaseURL + component.GoogleCalendarCallbackPath
}

// validGoogleState reports whether the callback's state matches the state
// cookie and that cookie was issued to userID.
func (app *application) validGoogleState(r *http.Request, userID int64) bool {
	got := r.URL.Query().Get("state")
	cookie, err := r.Cookie(googleStateCookie)
	if got == "" || err != nil || app.config.SessionSecret == "" {
		return false
	}
	state, mac, ok := strings.Cut(cookie.Value, ".")
	if !ok || state == "" {
		return false
	}
	if !hmac.Equal([]byte(state), []byte(got)) {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(app.googleStateMAC(userID, state)))
}

// handleGoogleCalendarCallback finishes a connect: it checks the state, trades
// the code for tokens and stores them for the user who started the connect.
// Every failure lands on the profile with the fixed failure flag and stores
// nothing.
func (app *application) handleGoogleCalendarCallback(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	userID, ok := app.sessionUserID(ctx, w, r)
	if !ok {
		return
	}
	// The state is single use whatever the outcome.
	http.SetCookie(w, googleStateCookieFor("", -1))
	fail := func(reason string) {
		fmt.Printf("google calendar callback failed: %s\n", reason)
		http.Redirect(w, r, calendarErrorPath, http.StatusSeeOther)
	}
	query := r.URL.Query()
	if !app.config.googleAvailable() {
		fail("google not configured")
		return
	}
	if query.Get("error") != "" {
		fail("google returned an error")
		return
	}
	if !app.validGoogleState(r, userID) {
		fail("state mismatch")
		return
	}
	code := query.Get("code")
	if code == "" {
		fail("missing code")
		return
	}
	conn, err := app.exchangeGoogleCode(r.Context(), code)
	if err != nil {
		fail(err.Error())
		return
	}
	conn.UserID = userID
	if err := app.store.SaveGoogleCalendarConnection(r.Context(), userID, conn); err != nil {
		fail("store: " + err.Error())
		return
	}
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

type googleTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"`
	// Scope is the space-delimited set Google actually granted, which can be
	// narrower than requested when the user unticks a scope on the consent
	// screen.
	Scope string `json:"scope"`
}

// googleCalendarReadScope is the grant a connection cannot work without.
const googleCalendarReadScope = "https://www.googleapis.com/auth/calendar.readonly"

// grantsGoogleCalendar reports whether a granted scope string includes
// calendar read access.
func grantsGoogleCalendar(scope string) bool {
	for _, s := range strings.Fields(scope) {
		if s == googleCalendarReadScope {
			return true
		}
	}
	return false
}

// exchangeGoogleCode trades an authorization code at the token endpoint. The
// returned errors never carry token values.
func (app *application) exchangeGoogleCode(ctx context.Context, code string) (db.GoogleCalendarConnection, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {app.config.GoogleClientID},
		"client_secret": {app.config.GoogleClientSecret},
		"redirect_uri":  {app.googleRedirectURI()},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return db.GoogleCalendarConnection{}, errors.New("token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := googleHTTPClient.Do(req)
	if err != nil {
		return db.GoogleCalendarConnection{}, errors.New("token exchange unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return db.GoogleCalendarConnection{}, fmt.Errorf("token exchange status %d", resp.StatusCode)
	}
	var tokens googleTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokens); err != nil {
		return db.GoogleCalendarConnection{}, errors.New("token response unreadable")
	}
	if tokens.AccessToken == "" && tokens.RefreshToken == "" {
		return db.GoogleCalendarConnection{}, errors.New("token response without tokens")
	}
	if !grantsGoogleCalendar(tokens.Scope) {
		// A partial grant is no connection: hand the just-issued token back
		// (best effort) and store nothing.
		token := tokens.RefreshToken
		if token == "" {
			token = tokens.AccessToken
		}
		if err := revokeGoogleToken(ctx, token); err != nil {
			fmt.Printf("google calendar revoke after partial grant (ignored): %s\n", err)
		}
		return db.GoogleCalendarConnection{}, errors.New("calendar scope not granted")
	}
	email, err := googleIDTokenEmail(tokens.IDToken)
	if err != nil {
		return db.GoogleCalendarConnection{}, err
	}
	conn := db.GoogleCalendarConnection{
		Email:        email,
		RefreshToken: tokens.RefreshToken,
		AccessToken:  tokens.AccessToken,
	}
	if tokens.ExpiresIn > 0 {
		conn.TokenExpiry = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	}
	return conn, nil
}

// googleIDTokenEmail reads the email claim of an id_token received directly
// from Google's token endpoint over TLS (OpenID Connect Core 3.1.3.7 allows
// skipping signature validation for a token obtained that way).
func googleIDTokenEmail(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", errors.New("id_token malformed")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", errors.New("id_token payload malformed")
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Email == "" {
		return "", errors.New("id_token without email")
	}
	return claims.Email, nil
}

// handleGoogleCalendarDisconnect deletes the user's stored connection, then
// asks Google to revoke the deleted token. Revocation is best effort and needs
// no client credentials; a delete failure leaves everything as it was.
func (app *application) handleGoogleCalendarDisconnect(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	userID, ok := app.sessionUserID(ctx, w, r)
	if !ok {
		return
	}
	conn, found, err := app.store.DeleteGoogleCalendarConnection(r.Context(), userID)
	if err != nil {
		fmt.Printf("google calendar disconnect: %s\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		if err := template.ErrorPage("Gather Your Party", "unable to disconnect Google Calendar").Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	if found {
		token := conn.RefreshToken
		if token == "" {
			token = conn.AccessToken
		}
		if err := revokeGoogleToken(r.Context(), token); err != nil {
			fmt.Printf("google calendar revoke (ignored): %s\n", err)
		}
	}
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

// revokeGoogleToken asks Google to revoke token. Its errors never carry the token.
func revokeGoogleToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleRevokeEndpoint, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return errors.New("revoke request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := googleHTTPClient.Do(req)
	if err != nil {
		return errors.New("revoke unreachable")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("revoke status %d", resp.StatusCode)
	}
	return nil
}
