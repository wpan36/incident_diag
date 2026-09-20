.DEFAULT_GOAL := check

GO ?= go

# ---- Local configuration ---------------------------------------------------

# `.env` holds the local environment: copy `.env.example` to it. It is
# git-ignored and must never be committed.
#
# It is loaded here, once, and exported to every recipe, so `make migrate-up`
# and `make test-integration` see MYSQL_DSN and TEST_MYSQL_DSN without anyone
# having to export them by hand. Do not try to `source .env` in a shell: the
# DSN contains `(`, `)` and `&`, which bash parses rather than takes literally.
#
# Because make, and docker compose, both parse this file directly, its values
# must stay unquoted `KEY=value` lines.
ENV_FILE ?= .env
ifneq ($(wildcard $(ENV_FILE)),)
include $(ENV_FILE)
export
endif

# Every milestone must pass `make check` before it is committed.
.PHONY: check
check: fmt-check vet test race

.PHONY: fmt
fmt:
	gofmt -w .

# Fails (listing offenders) instead of rewriting, so CI and commits can rely on it.
.PHONY: fmt-check
fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needed for:"; echo "$$out"; exit 1; \
	fi

# The go tool treats "./... matched no packages" as an error, which would break
# `make check` on a repo that has no Go code yet. Nothing to do is not a failure.
HAVE_PKGS = [ -n "$$($(GO) list ./... 2>/dev/null)" ]

.PHONY: vet
vet:
	@if $(HAVE_PKGS); then $(GO) vet ./...; else echo "no Go packages yet; skipping vet"; fi

.PHONY: test
test:
	@if $(HAVE_PKGS); then $(GO) test ./...; else echo "no Go packages yet; skipping test"; fi

.PHONY: race
race:
	@if $(HAVE_PKGS); then $(GO) test -race ./...; else echo "no Go packages yet; skipping race"; fi

# Integration tests need the docker compose infrastructure running. They skip
# cleanly when it is absent, so `make test` stays green either way.
#
# -p 1 runs one package at a time. Every package here shares the one test
# database, and one of the tests drops every table to check that the down
# migration works, which cannot happen while another package is querying them.
# Running `go test -tags=integration ./...` by hand without this flag is the
# one way to see these tests fail spuriously.
.PHONY: test-integration
test-integration:
	$(GO) test -p 1 -tags=integration ./...

# ---- Local infrastructure --------------------------------------------------

# --project-directory is not optional. Without it compose takes the project
# directory from the compose file's own location, reads `deploy/.env`, and
# silently ignores the `.env` at the repository root — so the server would come
# up on the built-in defaults while the Go binaries used the DSN from a file
# compose never saw. The project name is pinned inside the compose file, so
# moving the project directory does not rename anything.
COMPOSE ?= docker compose -f deploy/docker-compose.yml --project-directory .

.PHONY: up
up:
	$(COMPOSE) up -d --wait

.PHONY: down
down:
	$(COMPOSE) down

# Also removes the volumes, so the next `make up` starts from an empty database.
.PHONY: down-clean
down-clean:
	$(COMPOSE) down -v

.PHONY: logs
logs:
	$(COMPOSE) logs -f

# ---- Migrations ------------------------------------------------------------

.PHONY: migrate-up
migrate-up:
	$(GO) run ./cmd/migrate up

.PHONY: migrate-down
migrate-down:
	$(GO) run ./cmd/migrate down

.PHONY: tidy
tidy:
	$(GO) mod tidy
