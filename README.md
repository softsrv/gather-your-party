# Gather Your Party

Help your plan game nights with your friends.  Agree on a time, place, and game ahead of time to maximize time spent playing with your friends.

## Configuration and database setup

Use Go 1.27.1 (the version in `go.mod`; newer Go toolchains download it automatically). Provision an externally hosted Postgres database (for example,
Neon, Supabase, or Amazon RDS) before first use. The application does not provision
a database. On boot it verifies connectivity and applies embedded migrations in
filename order before starting HTTP, tracking applied files in `schema_migrations`.

Copy `.env.example` to `.env` and replace its placeholders, set `ENV_FILE_PATH` to
another env file (such as `/etc/secrets/.env`), or export these variables in the
process environment. Existing environment variables take precedence over file
values loaded by godotenv. With no non-empty `ENV_FILE_PATH`, `.env` is loaded from
the working directory; missing env files are optional.

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Provider's Postgres connection URL, including required TLS options. |
| `SESSION_SECRET` | Required high-entropy secret that signs the session cookie with HMAC-SHA256. |
| `APP_BASE_URL` | Public origin used for the Steam OpenID realm/return_to, including behind a TLS-terminating reverse proxy (e.g. `https://example.com`, no trailing slash). |
| `STEAM_API_KEY` | Steam Web API key for profile and game requests. |
| `LISTEN_ADDR` | HTTP port, e.g. `8080` (not a host:port pair). |

For example, set `DATABASE_URL` to
`postgres://user:pass@host:5432/db?sslmode=require`. Use the TLS settings required
by your provider; the application passes this URL unchanged to pgxpool and pings
the pool before migrating. Connection or migration failures prevent serving.

Do not apply SQL manually on a new database: startup applies each unrecorded
`migrations/*.sql` file and records its filename in the same transaction. Subsequent
boots skip recorded files. Migration files are embedded at build time, so rebuild
the binary when adding migrations. New migrations should contain transactional SQL
without transaction-control statements; the runner also handles the existing
files' outer `BEGIN;`/`COMMIT;` envelopes.

**Upgrading a database created before migration tracking:** the runner cannot infer
which SQL files were applied manually. Back up the database, audit its schema, and
baseline `schema_migrations` with only the filenames confirmed already applied
before starting this version. Do not blindly mark all files applied. For disposable
local data only, `make db-reset` deletes the old database; the next `make dev` boot
creates its schema through the tracked runner. The dev database no longer uses the
Postgres initdb hook to apply untracked SQL.

The first migration creates users keyed by a unique SteamID64 and sessions with a
user foreign key and expiry. `internal/db` exposes an upsert that requires a
caller-verified SteamID64 and session creation using a random 256-bit token,
expiring after seven days. The Steam OpenID callback validates the assertion
with Steam, fetches the verified user's profile, upserts the user, and creates a
session. It sets the opaque token with an HMAC-SHA256 signature in an HttpOnly,
Secure, SameSite=Lax cookie. Set `SESSION_SECRET` to a high-entropy secret and
`APP_BASE_URL` to the public origin used for the OpenID realm and callback URL;
request Host and proxy headers are not used to construct that origin. Deployed
sign-in requires HTTPS, including when TLS terminates at a reverse proxy.
The old Steam-ID entry form and GET/POST `/login` handlers have been removed.
Authenticated requests verify the cookie signature, resolve the verified SteamID64,
and refresh both the database expiry and browser cookie to seven days from now.
Expired or invalid sessions fall through to Steam sign-in. The signed-in navigation
submits a POST to `/auth/steam/logout`, which deletes the session and clears its cookie.

## Verification

```sh
go mod tidy
go build ./...
go vet ./...
go test ./...
gofmt -l .
```

The database integration tests need Docker. Each test package starts its own
throwaway `postgres:16-alpine` container on the first free port from 55432 up
(checked before Docker binds it) and removes it when the tests finish:

```sh
make test-integration   # go test -tags=integration ./... -count=1
```

To test against an existing database instead, set `TEST_DATABASE_URL`; it must be
a **disposable, empty** database, since the tests apply the migrations and drop
their tables on cleanup. Never point it at a shared or production database. The
URL is passed unchanged to pgxpool, so a TLS-requiring database can be used to
check the provider's TLS settings too.

```sh
TEST_DATABASE_URL='postgres://user:pass@host:5432/test_db?sslmode=require' \
  go test -tags=integration ./... -count=1 -v
```

