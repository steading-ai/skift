# Build, test, and lint for skift.
#
# Tools come from mise.toml. Run `mise install` once; the shims put every tool
# used below on PATH.

MODULE  := github.com/steading-ai/skift
BINARY  := skift

# Version is the tag when the tree is exactly at a v-prefixed tag and clean,
# and otherwise the nearest tag carrying a development marker: the commits
# since it, the commit, and .dirty for local changes, as build metadata so
# the string stays semver-shaped. Without a tag the nearest is 0.0.0, and
# without git at all the marker is all there is.
#
#   v0.1.0, clean          0.1.0
#   3 commits past v0.1.0  0.1.0+dev.3.g1a2b3c4
#   the same, edited       0.1.0+dev.3.g1a2b3c4.dirty
GIT_DESCRIBE := $(shell git describe --tags --match 'v*' --long --dirty 2>/dev/null || \
  { git rev-parse --verify -q HEAD >/dev/null 2>&1 && \
    echo "v0.0.0-$$(git rev-list --count HEAD)-g$$(git rev-parse --short HEAD)$$(git diff-index --quiet HEAD -- || echo -dirty)"; })
VERSION      ?= $(if $(GIT_DESCRIBE),$(shell echo '$(GIT_DESCRIBE)' | sed -E \
  's/^v//; s/^([^-]+)-0-g[0-9a-f]+$$/\1/; s/^([^-]+)-([0-9]+)-g([0-9a-f]+)$$/\1+dev.\2.g\3/; s/^([^-]+)-([0-9]+)-g([0-9a-f]+)-dirty$$/\1+dev.\2.g\3.dirty/'),0.0.0+dev.unknown)

LDFLAGS := -X $(MODULE)/internal/version.version=$(VERSION)

GINKGO := go tool ginkgo -r -p --randomize-all --fail-on-pending --keep-going

COMPOSE := podman compose
# Silence podman's "executing external compose provider" banner.
export PODMAN_COMPOSE_WARNING_LOGS = false

.PHONY: all build version cover mutate test test-unit test-db test-acceptance test-policy lint check-generated generate clean package db-up db-down db-reset db-migrate

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/skift

# The version the next build would carry, as skift version will report it.
version:
	@echo $(VERSION)

# Every spec, and the policy bundle's own suite. Database specs need
# podman for testcontainers.
test: test-policy
	$(GINKGO) ./...

# Specs that run without external services. DOCKER_HOST names a socket
# nobody listens on, so a unit spec that starts a container fails here
# rather than passing on a machine where podman happens to be running.
test-unit: test-policy
	DOCKER_HOST=unix:///nonexistent/podman.sock TESTCONTAINERS_RYUK_DISABLED=true \
	  $(GINKGO) --label-filter='!db' ./...

# The Rego suite beside the default policy bundle.
test-policy:
	opa test policy

# Every spec under the race detector. Claims, leases, the event listener
# and the watch loop all run goroutines against one database; the
# detector is what notices a data race the specs happen not to trip.
test-race: test-policy
	$(GINKGO) -race ./...

# The fuzz targets, each for FUZZ_TIME. go test drives these directly and
# one at a time, so they live in *_fuzz_test.go beside the specs; the
# corpus of anything that ever failed is committed under testdata/fuzz.
FUZZ_TIME ?= 20s
fuzz:
	go test ./internal/codec -run '^$$' -fuzz '^FuzzUnmarshal$$' -fuzztime $(FUZZ_TIME)
	go test ./internal/domain -run '^$$' -fuzz '^FuzzDecodeCursor$$' -fuzztime $(FUZZ_TIME)

# Specs that need a real PostgreSQL, labelled "db".
test-db:
	$(GINKGO) --label-filter='db' ./...

# The end-to-end scenarios: the built binary serving a fresh database,
# driven through the CLI and the MCP server. Needs podman, like test-db.
test-acceptance:
	$(GINKGO) ./tests/acceptance

# Statement coverage of hand-written code, counting every spec and the
# acceptance scenarios. The scenarios run the built binary in its own
# process, so it is built with -cover and writes its counters to GOCOVERDIR,
# which the specs pass through. The two profiles merge block by block; files
# matching coverage.exclude are left out; and the result must not fall below
# coverage.baseline. Raise the baseline when coverage rises.
COVER_DIR := .cover
COVER_PKG := $(MODULE)/...

cover: test-policy
	rm -rf $(COVER_DIR) && mkdir -p $(COVER_DIR)/bin
	$(GINKGO) --skip-package=acceptance --cover --coverpkg=$(COVER_PKG) \
	  --coverprofile=specs.out --output-dir=$(COVER_DIR) ./...
	GOFLAGS='-cover -coverpkg=$(COVER_PKG)' GOCOVERDIR=$(abspath $(COVER_DIR)/bin) \
	  $(GINKGO) ./tests/acceptance
	go tool covdata textfmt -i=$(COVER_DIR)/bin -o=$(COVER_DIR)/acceptance.out
	@awk 'FNR == 1 { next } { k = $$1 " " $$2; c = ($$3 > 0); if (!(k in m) || c > m[k]) m[k] = c } \
	  END { print "mode: set"; for (k in m) print k, m[k] }' \
	  $(COVER_DIR)/specs.out $(COVER_DIR)/acceptance.out > $(COVER_DIR)/merged.out
	@grep -v -E '^(#|$$)' coverage.exclude > $(COVER_DIR)/exclude
	@{ head -1 $(COVER_DIR)/merged.out; tail -n +2 $(COVER_DIR)/merged.out | \
	  grep -v -E -f $(COVER_DIR)/exclude; } > $(COVER_DIR)/cover.out
	@awk -v base="$$(cat coverage.baseline)" 'FNR == 1 { next } \
	  { t += $$2; if ($$3 > 0) c += $$2 } \
	  END { p = int(1000 * c / t) / 10; \
	    printf "cover: %.1f%% of %d hand-written statements (baseline %.1f%%)\n", p, t, base; \
	    if (p < base) { print "cover: below coverage.baseline"; exit 1 } }' $(COVER_DIR)/cover.out
	@echo "cover: go tool cover -html=$(COVER_DIR)/cover.out for the detail"

# Mutation testing over the domain. gremlins changes one covered
# statement at a time and reruns the domain suite; a mutant the suite
# lets live is a check the specs do not make. Each mutant is a full run
# against its own PostgreSQL, so this takes hours and is not part of test
# or cover: run it when the domain's specs change shape, and read the
# survivors, not the score. Settings are in .gremlins.yaml.
#
# The timeout gremlins gives each mutant is a multiple of the unmutated
# run, and go test answers a cached run in milliseconds, so the cache is
# off: every mutant would time out against a baseline that measured nothing.
#
# Every worker's suite attaches to the development database rather than
# starting a container of its own: that many testcontainers sessions at
# once race on podman's networks, and a teardown that loses the race fails
# the run, which gremlins would count as a kill. Each suite still works in
# a template database of its own and drops it when it ends.
#
# The property specs' thousand histories are most of a serial run, and a
# mutant pays for a full run; PROPERTY_CHECKS=100 keeps the invariants in
# play at a third of the cost, and 1000 is the suite's own strength.
# DIFF=<branch or commit> mutates only the lines that diff touches, which
# is the everyday case.
PROPERTY_CHECKS ?= 100
MUTATE_FLAGS := $(if $(DIFF),--diff $(DIFF),)
mutate: db-up
	set -a && . ./.env && set +a && \
	  SKIFT_TEST_POSTGRES_URL="$$DATABASE_URL" SKIFT_PROPERTY_CHECKS=$(PROPERTY_CHECKS) GOFLAGS=-count=1 \
	  gremlins unleash $(MUTATE_FLAGS) ./internal/domain

lint:
	go vet ./...
	golangci-lint run ./...
	buf lint
	opa check --strict policy
	opa fmt --fail --list policy
	$(MAKE) --no-print-directory check-generated

# Generated code is committed, so a consumer needs no proto or SQL toolchain.
# Proto output and the descriptor set internal/protodoc reads are
# regenerated into a scratch directory and compared against the tree; sqlc
# compares its own output in place. Either lagging its source fails the
# build. Specs living beside generated packages are hand-written and
# excluded from the comparison.
check-generated:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	  buf generate -o "$$tmp" && \
	  buf build --as-file-descriptor-set -o "$$tmp/descriptors.binpb" && \
	  if ! diff -r --exclude='*_test.go' "$$tmp/gen" gen || \
	     ! cmp -s "$$tmp/descriptors.binpb" internal/protodoc/descriptors.binpb; then \
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
	buf build --as-file-descriptor-set -o internal/protodoc/descriptors.binpb
	sqlc generate

clean:
	rm -f $(BINARY)
	rm -rf dist

# The rpm, for each architecture in PACKAGE_ARCHES: a cross-compiled skift
# and the units, fragments and scriptlets under contrib/ and packaging/,
# assembled by nfpm from packaging/nfpm.yaml. Output lands in dist/.
PACKAGE_ARCHES ?= amd64 arm64
# Where the finished rpms go. stead passes its channel directory; staging
# stays under dist/ regardless.
DESTDIR ?= dist

package: $(foreach arch,$(PACKAGE_ARCHES),dist/skift-linux-$(arch))
	mkdir -p $(DESTDIR)
	for arch in $(PACKAGE_ARCHES); do \
	  GOARCH=$$arch VERSION=$(VERSION) nfpm package --config packaging/nfpm.yaml --packager rpm --target $(DESTDIR)/ || exit 1; \
	done

dist/skift-linux-%: FORCE
	CGO_ENABLED=0 GOOS=linux GOARCH=$* go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/skift

FORCE:

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
