//go:build integration

package db

import (
	"testing"

	"gather-your-party/internal/testdb"
)

// Integration tests use TEST_DATABASE_URL when set, otherwise a throwaway
// Postgres container that is removed afterwards.
func TestMain(m *testing.M) {
	testdb.Main(m)
}
