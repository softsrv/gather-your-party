package syncreason

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func steamTransportErr() error {
	return fmt.Errorf("steamapi: performing request: %w", &url.Error{
		Op:  "Get",
		URL: "https://api.steampowered.com/ISteamUser/GetFriendList/v1/?key=SECRETKEY123&steamid=1",
		Err: errors.New("dial tcp: connection refused"),
	})
}

func TestReasonDropsURLAndKeepsCause(t *testing.T) {
	got := Reason(fmt.Errorf("sync games 1: %w", steamTransportErr()))
	if !strings.Contains(got, "connection refused") || !strings.Contains(got, "steamapi: performing request") {
		t.Fatalf("reason lost its cause: %q", got)
	}
	if strings.Contains(got, "SECRETKEY123") || strings.Contains(got, "api.steampowered.com") {
		t.Fatalf("reason leaks the request URL: %q", got)
	}
}

func TestReasonDeadlineIsTimeout(t *testing.T) {
	if got := Reason(fmt.Errorf("x: %w", context.DeadlineExceeded)); got != TimeoutReason {
		t.Fatalf("got %q", got)
	}
}

func TestReasonPlainErrorPassesThrough(t *testing.T) {
	if got := Reason(errors.New("steam said no")); got != "steam said no" {
		t.Fatalf("got %q", got)
	}
	if got := Reason(nil); got != "" {
		t.Fatalf("nil error: %q", got)
	}
}

func TestScrubRedactsKeyAndQueryStrings(t *testing.T) {
	in := `bad https://api.steampowered.com/a/b?key=SECRETKEY123&x=1 and /p?foo=1&KEY=SECRETKEY123 "key=SECRETKEY123"`
	got := Scrub(in)
	if strings.Contains(got, "SECRETKEY123") {
		t.Fatalf("scrub leaks: %q", got)
	}
	if !strings.Contains(got, "https://api.steampowered.com/a/b") {
		t.Fatalf("scrub dropped the URL path: %q", got)
	}
}

func TestScrubCapsLength(t *testing.T) {
	if got := []rune(Scrub(strings.Repeat("é", 1000))); len(got) > maxLen+1 {
		t.Fatalf("len %d", len(got))
	}
}
