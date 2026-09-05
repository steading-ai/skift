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

.PHONY: all build test test-unit test-db lint generate clean

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
