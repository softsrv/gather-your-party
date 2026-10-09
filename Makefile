# Run templ at the version pinned in go.mod so generated code matches the runtime.
TEMPL_VERSION := $(shell go list -m -f '{{.Version}}' github.com/a-h/templ)
TEMPL := go run github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION)

# Tailwind v4 provides its CLI separately; daisyUI is loaded from the CSS entrypoint.
TAILWIND := npx --yes @tailwindcss/cli@latest

# Local dev Postgres. Matches DATABASE_URL in .env:
#   postgres://gyp:gyp@localhost:54329/gather_your_party?sslmode=disable
DB_CONTAINER := gather-your-party-db
DB_VOLUME := gather-your-party-pgdata
DB_PORT := 54329
DB_USER := gyp
DB_PASSWORD := gyp
DB_NAME := gather_your_party

# Hot reloader, installed into tmp/bin. It's built rather than run with `go run`,
# which doesn't forward SIGTERM/SIGHUP and would orphan the server when make exits.
AIR_VERSION := v1.67.4
AIR := tmp/bin/air-$(AIR_VERSION)

.PHONY: dev run templ tw db db-stop db-reset test test-integration

# Start Postgres, then run the app locally (reads .env) under air, which regenerates
# templates/CSS and rebuilds + restarts the server on .go/.templ changes (see .air.toml).
dev: db $(AIR)
	@test -f .env || { echo "missing .env: copy .env.example to .env and fill it in"; exit 1; }
	exec ./$(AIR)

$(AIR):
	GOBIN=$(CURDIR)/tmp/bin go install github.com/air-verse/air@$(AIR_VERSION)
	mv tmp/bin/air $@

run: dev

templ:
	$(TEMPL) generate

tw:
	$(TAILWIND) -i ./static/css/input.css -o ./static/css/output.css

# Create (or restart) Postgres. The application applies and tracks migrations
# on boot; do not pre-apply raw SQL via the image's initdb hook.
db:
	@if [ -z "$$(docker ps -aq -f name=^$(DB_CONTAINER)$$)" ]; then \
		docker run -d --name $(DB_CONTAINER) \
			-e POSTGRES_USER=$(DB_USER) -e POSTGRES_PASSWORD=$(DB_PASSWORD) -e POSTGRES_DB=$(DB_NAME) \
			-p 127.0.0.1:$(DB_PORT):5432 \
			-v $(DB_VOLUME):/var/lib/postgresql/data \
			postgres:16-alpine >/dev/null; \
	else \
		docker start $(DB_CONTAINER) >/dev/null; \
	fi
	@echo "waiting for postgres on localhost:$(DB_PORT)..."
	@until docker exec $(DB_CONTAINER) pg_isready -q -h 127.0.0.1 -U $(DB_USER) -d $(DB_NAME); do sleep 1; done

db-stop:
	-docker stop $(DB_CONTAINER)

# Delete the container and its data, then recreate it empty for the next app boot.
db-reset:
	-docker rm -f $(DB_CONTAINER)
	-docker volume rm $(DB_VOLUME)
	$(MAKE) db

test:
	go test ./...

# Starts a throwaway Postgres container per package on a free port (from 55432 up)
# unless TEST_DATABASE_URL is set; see internal/testdb.
test-integration:
	go test -tags=integration ./... -count=1
