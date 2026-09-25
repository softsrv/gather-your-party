//go:build integration

package view

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// Exercise action authorization against real ResolveUserID and lifecycle writes.
func TestPartyActionsIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must name an empty, disposable Postgres database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	defer admin.Close()
	schema := fmt.Sprintf("party_actions_test_%d", time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot configure scoped test pool")
	}
	defer pool.Close()
	for _, path := range []string{"../../migrations/0001_init.sql", "../../migrations/0002_parties.sql"} {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	store := db.New(pool)
	const steamID = "76561198000000001"
	leader, err := store.UpsertUser(ctx, steamID, steamapi.Player{PersonaName: "Leader"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.UpsertUser(ctx, "76561198000000002", steamapi.Player{PersonaName: "Member"})
	if err != nil {
		t.Fatal(err)
	}
	actions := PartyActions{Store: store}
	request := func(handler middleware.CustomHandler, identity any, id string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		requestCtx := ctx
		if identity != nil {
			requestCtx = context.WithValue(ctx, middleware.SteamID{}, identity)
		}
		r := httptest.NewRequest(http.MethodPost, "/parties", strings.NewReader(form.Encode())).WithContext(ctx)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		r.SetPathValue("partyID", id)
		w := httptest.NewRecorder()
		handler(&middleware.CustomContext{Context: requestCtx}, w, r)
		return w
	}
	assertInt := func(want int64, sql string, args ...any) {
		t.Helper()
		var got int64
		if err := pool.QueryRow(ctx, sql, args...).Scan(&got); err != nil || got != want {
			t.Fatalf("query returned %d, want %d, error %v", got, want, err)
		}
	}
	form := url.Values{"name": {"Party"}, "targetUserID": {strconv.FormatInt(member, 10)}, "userID": {strconv.FormatInt(member, 10)}}
	created := request(actions.CreateParty, steamID, "", form)
	if created.Code != http.StatusNoContent || created.Header().Get("HX-Redirect") != "/parties" {
		t.Fatalf("create response: %d %s", created.Code, created.Body.String())
	}
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM parties WHERE name = 'Party'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	assertInt(leader, `SELECT leader_id FROM parties WHERE id = $1`, id)
	assertInt(1, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2`, id, leader)

	for _, identity := range []any{nil, "", "76561198999999999", int64(1)} {
		for _, handler := range []middleware.CustomHandler{actions.CreateParty, actions.LeaveParty, actions.StepDown} {
			w := request(handler, identity, id, form)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("identity %v got status %d", identity, w.Code)
			}
			assertInt(1, `SELECT count(*) FROM parties`)
			assertInt(leader, `SELECT leader_id FROM parties WHERE id = $1`, id)
			assertInt(1, `SELECT count(*) FROM memberships WHERE party_id = $1`, id)
		}
	}
	// Valid identity cannot step down in a party of one, even to itself.
	self := url.Values{"targetUserID": {strconv.FormatInt(leader, 10)}}
	if w := request(actions.StepDown, steamID, id, self); w.Code != http.StatusForbidden {
		t.Fatalf("size-one response = %d", w.Code)
	}
	assertInt(leader, `SELECT leader_id FROM parties WHERE id = $1`, id)
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, id, member); err != nil {
		t.Fatal(err)
	}
	if w := request(actions.StepDown, "76561198000000002", id, self); w.Code != http.StatusForbidden {
		t.Fatalf("non-leader response = %d", w.Code)
	}
	assertInt(leader, `SELECT leader_id FROM parties WHERE id = $1`, id)
	if w := request(actions.StepDown, steamID, id, form); w.Code != http.StatusNoContent {
		t.Fatalf("step-down response = %d", w.Code)
	}
	assertInt(member, `SELECT leader_id FROM parties WHERE id = $1`, id)
	assertInt(1, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2`, id, leader)

	ordinary := request(actions.LeaveParty, steamID, id, form)
	assertInt(1, `SELECT count(*) FROM parties WHERE id = $1`, id)
	assertInt(0, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2`, id, leader)
	assertInt(1, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2`, id, member)
	last := request(actions.LeaveParty, "76561198000000002", id, form)
	assertInt(0, `SELECT count(*) FROM parties WHERE id = $1`, id)
	if ordinary.Code != http.StatusNoContent || last.Code != ordinary.Code || last.Body.String() != ordinary.Body.String() || last.Header().Get("HX-Redirect") != ordinary.Header().Get("HX-Redirect") {
		t.Fatalf("leave responses differ: ordinary=%+v, last=%+v", ordinary, last)
	}
	if last.Body.Len() != 0 || last.Header().Get("HX-Redirect") != "/parties" {
		t.Fatal("leave must return only the generic redirect, without a notice")
	}
}
