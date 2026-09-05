# Build, test, and lint for dagger.
#
# Tools come from mise.toml. Run `mise install` once; the shims put every tool
# used below on PATH.

MODULE  := github.com/ivy/dagger
BINARY  := dg

# Version is the nearest v-prefixed tag, marked dirty when the tree has local
# changes. Without a tag the build is a 0.0.0 pre-release carrying the commit,
# so the string is always semver-shaped.
GIT_DESCRIBE := $(shell git describe --tags --match 'v*' --dirty 2>/dev/null | sed 's/^v//')
GIT_SHA      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
VERSION      ?= $(if $(GIT_DESCRIBE),$(GIT_DESCRIBE),0.0.0-dev+$(GIT_SHA))

LDFLAGS := -X $(MODULE)/internal/version.version=$(VERSION)

GINKGO := go tool ginkgo -r -p --randomize-all --fail-on-pending --keep-going

COMPOSE := podman compose
# Silence podman's "executing external compose provider" banner.
export PODMAN_COMPOSE_WARNING_LOGS = false

.PHONY: all build test test-unit test-db test-policy lint check-generated generate clean install-units db-up db-down db-reset db-migrate

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/dg

# Every spec, and the policy bundle's own suite. Database specs need
# podman for testcontainers.
test: test-policy
	$(GINKGO) ./...

# Specs that run without external services.
test-unit: test-policy
	$(GINKGO) --label-filter='!db' ./...

# The Rego suite beside the default policy bundle.
test-policy:
	opa test policy

# Specs that need a real PostgreSQL, labelled "db".
test-db:
	$(GINKGO) --label-filter='db' ./...

lint:
	go vet ./...
	golangci-lint run ./...
	buf lint
	opa check --strict policy
	opa fmt --fail --list policy
	$(MAKE) --no-print-directory check-generated

# Generated code is committed, so a consumer needs no proto or SQL toolchain.
# Proto output is regenerated into a scratch directory and diffed against
# gen/; sqlc compares its own output in place. Either lagging its source
# fails the build. Specs living beside generated packages are hand-written
# and excluded from the comparison.
check-generated:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	  buf generate -o "$$tmp" && \
	  if ! diff -r --exclude='*_test.go' "$$tmp/gen" gen; then \
	    echo "lint: generated code is stale relative to proto/; run make generate"; \
	    exit 1; \
	  fi
	@if ! sqlc diff; then \
	  echo "lint: generated code is stale relative to internal/store/queries/; run make generate"; \
	  exit 1; \
	fi

# Code generators. Each is deterministic: running twice leaves the tree
# unchanged, which check-generated relies on.
generate:
	go generate ./...
	buf generate
	sqlc generate

clean:
	rm -f $(BINARY)
	rm -rf dist

# systemd user units for the service and the lease reaper. They go under
# the invoking user's unit directory, and are enabled by hand afterwards:
#
#   systemctl --user enable --now dagger.service dagger-reap.timer
#
# The units start the binary at $(DG), which defaults to ~/.local/bin/dg
# spelled as systemd spells the home directory. Override it to run a binary
# from somewhere else, such as DG=$(CURDIR)/dg for this checkout.
UNIT_DIR ?= $(if $(XDG_CONFIG_HOME),$(XDG_CONFIG_HOME),$(HOME)/.config)/systemd/user
DG       ?= %h/.local/bin/dg

install-units:
	install -d -m 0755 "$(UNIT_DIR)"
	for unit in contrib/systemd/*; do \
	  sed 's|%h/.local/bin/dg|$(DG)|' "$$unit" > "$(UNIT_DIR)/$$(basename "$$unit")"; \
	done
	systemctl --user daemon-reload

# Development database. See compose.yaml and the "Developing" section of README.md.

# .env carries the development credentials and is gitignored. Seed it from the
# example on a fresh clone so db-up works out of the box.
.env:
	cp .env.example .env

# Start PostgreSQL and block until it accepts TCP connections.
db-up: .env
	$(COMPOSE) up -d --wait --wait-timeout 30

# Stop the stack. The data volume survives; `podman compose down -v` removes it.
db-down:
	$(COMPOSE) down

# Apply every pending migration to the development database. The service
# and the specs embed the same files; this is for working on the schema by
# hand. DATABASE_URL comes from .env.
db-migrate: db-up
	set -a && . ./.env && set +a && \
	  goose -dir internal/store/migrations postgres "$$DATABASE_URL" up

# Drop and recreate the development database inside the running container.
# Credentials resolve from the container's environment, so this file never
# needs to read .env.
db-reset: db-up
	$(COMPOSE) exec -T postgres sh -euc '\
	  psql -v ON_ERROR_STOP=1 -U "$$POSTGRES_USER" -d postgres \
	    -c "DROP DATABASE IF EXISTS $$POSTGRES_DB WITH (FORCE)" \
	    -c "CREATE DATABASE $$POSTGRES_DB"'
