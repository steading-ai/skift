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

GINKGO := go tool ginkgo -r --randomize-all --fail-on-pending --keep-going

COMPOSE := podman compose
# Silence podman's "executing external compose provider" banner.
export PODMAN_COMPOSE_WARNING_LOGS = false

.PHONY: all build test test-unit test-db lint generate clean db-up db-down db-reset

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/dg

# Every spec. Database specs need podman for testcontainers.
test:
	$(GINKGO) ./...

# Specs that run without external services.
test-unit:
	$(GINKGO) --label-filter='!db' ./...

# Specs that need a real PostgreSQL, labelled "db".
test-db:
	$(GINKGO) --label-filter='db' ./...

lint:
	go vet ./...
	golangci-lint run ./...

# Code generators. buf and sqlc run only once their configs exist, so the
# target is safe before the proto and schema nodes land.
generate:
	go generate ./...
	$(if $(wildcard buf.gen.yaml),buf generate,@echo "generate: no buf.gen.yaml, skipping buf")
	$(if $(wildcard sqlc.yaml),sqlc generate,@echo "generate: no sqlc.yaml, skipping sqlc")

clean:
	rm -f $(BINARY)
	rm -rf dist

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

# Drop and recreate the development database inside the running container.
# Credentials resolve from the container's environment, so this file never
# needs to read .env.
db-reset: db-up
	$(COMPOSE) exec -T postgres sh -euc '\
	  psql -v ON_ERROR_STOP=1 -U "$$POSTGRES_USER" -d postgres \
	    -c "DROP DATABASE IF EXISTS $$POSTGRES_DB WITH (FORCE)" \
	    -c "CREATE DATABASE $$POSTGRES_DB"'
