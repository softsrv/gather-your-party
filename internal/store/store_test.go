package store

import (
	"testing"
	"time"
)

// TestSessionExpiry_Is30DaysAfterNow asserts that sessionExpiry returns a time
// exactly 30 days after the given reference time (CLM-2).
func TestSessionExpiry_Is30DaysAfterNow(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	want := t0.Add(30 * 24 * time.Hour)

	got := sessionExpiry(t0)

	if !got.Equal(want) {
		t.Errorf("sessionExpiry(%v) = %v, want %v", t0, got, want)
	}
}

// TestSessionExpiry_ExactDuration asserts the duration between the reference
// time and the expiry is exactly 30*24 hours (CLM-2, belt-and-suspenders).
func TestSessionExpiry_ExactDuration(t *testing.T) {
	t0 := time.Now()
	expiry := sessionExpiry(t0)
	diff := expiry.Sub(t0)
	want := 30 * 24 * time.Hour

	if diff != want {
		t.Errorf("sessionExpiry duration = %v, want %v", diff, want)
	}
}
