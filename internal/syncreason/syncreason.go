// Package syncreason turns a library-sync error into the short, secret-free
// reason that is stored and shown to users. The Steam client puts the API key
// in request URLs and its transport errors embed them, so a raw error text must
// never reach storage or HTML.
package syncreason

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// TimeoutReason is the stored and shown reason of a sync that ran out of time.
const TimeoutReason = "library save timed out"

// maxLen caps a reason, in runes.
const maxLen = 300

var (
	// urlQuery matches an http(s) URL with its query string or fragment.
	urlQuery = regexp.MustCompile(`(?i)(https?://[^\s"'?#]*)[?#][^\s"']*`)
	// keyParam matches a key=<value> pair wherever it remains, in a query
	// string or bare. It matches key= at the end of any parameter name
	// (api_key, steam_key, apikey, api-key), and words like monkey= are
	// redacted too, which is accepted.
	keyParam = regexp.MustCompile(`(?i)(key=)[^&\s"']+`)
)

// Reason returns the reason for a failed sync. A deadline maps to
// TimeoutReason; a *url.Error is reported as its operation and inner error,
// never its URL; the result is scrubbed and capped.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return TimeoutReason
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue != nil {
		inner := "request failed"
		if ue.Err != nil {
			inner = ue.Err.Error()
		}
		msg = strings.ReplaceAll(msg, ue.Error(), strings.TrimSpace(ue.Op+": "+inner))
	}
	return Scrub(msg)
}

// Scrub removes URL query strings and key= parameters from s and caps its
// length. It is safe to apply to an already stored reason.
func Scrub(s string) string {
	s = urlQuery.ReplaceAllString(s, "$1")
	s = keyParam.ReplaceAllString(s, "${1}REDACTED")
	if r := []rune(s); len(r) > maxLen {
		s = string(r[:maxLen]) + "…"
	}
	return s
}
