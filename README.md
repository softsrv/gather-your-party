# Gather Your Party

Help your plan game nights with your friends.  Agree on a time, place, and game ahead of time to maximize time spent playing with your friends.

## Configuration and database setup

Use Go 1.22.3. Provision an externally hosted Postgres database (for example,
Neon, Supabase, or Amazon RDS) before first use. The application does not provision
a database or run migrations automatically.

Copy `.env.example` to `.env` and replace its placeholders, or export these
variables in the process environment. Existing environment variables take
precedence over `.env` values loaded by godotenv.

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Provider's Postgres connection URL, including required TLS options. |
| `SESSION_SECRET` | High-entropy secret reserved for the upcoming session-security code. |
| `APP_BASE_URL` | Public application origin, reserved for upcoming authentication callbacks. |
| `STEAM_API_KEY` | Steam Web API key for profile and game requests. |
| `LISTEN_ADDR` | HTTP port, e.g. `8080` (not a host:port pair). |

For example, set `DATABASE_URL` to
`postgres://user:pass@host:5432/db?sslmode=require`. Use the TLS settings required
by your provider; the application passes this URL unchanged to pgxpool. Pool
creation is lazy: it does not guarantee connectivity until a query is made.

Before first use, export `DATABASE_URL` in your shell (`psql` does not load `.env`)
and apply the migration **once**, from the repository root:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/0001_init.sql
```

The migration creates users keyed by a unique SteamID64 and sessions with a
user foreign key and expiry. `internal/db` exposes an upsert that requires a
caller-verified SteamID64 and session creation using a random 256-bit token,
expiring after seven days. Neither write path is called by a route yet.
The secret and public origin are retained in application configuration for later
consumers; this change does not implement authentication or secure the existing
login flow. Do not treat this persistence layer as completed authentication.

## Verification

```sh
go mod tidy
go build ./...
go vet ./...
go test ./...
gofmt -l .
```

The database integration tests require a **disposable, empty** Postgres database.
They apply the migration and drop its tables on cleanup; never point them at a
shared or production database. The URL is passed unchanged to pgxpool, so a
TLS-requiring database can be used to check the provider's TLS settings too.

```sh
TEST_DATABASE_URL='postgres://user:pass@host:5432/test_db?sslmode=require' \
  go test -tags=integration ./internal/db -count=1 -v
```

