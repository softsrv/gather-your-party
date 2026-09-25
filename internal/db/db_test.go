package db

import (
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNewRetainsPool(t *testing.T) {
	pool := &pgxpool.Pool{}
	if New(pool).pool != pool {
		t.Fatal("store does not retain the supplied pool")
	}
}

func TestSessionTokens(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		token, err := newSessionToken()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := hex.DecodeString(token)
		if err != nil || len(decoded) != 32 {
			t.Fatalf("expected a hex-encoded 256-bit token: length %d, error %v", len(decoded), err)
		}
		if seen[token] {
			t.Fatal("generated duplicate token")
		}
		seen[token] = true
	}
}
