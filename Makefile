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
#   v0.2.0-rc.1, clean     0.2.0-rc.1
GIT_DESCRIBE := $(shell git describe --tags --match 'v*' --long --dirty 2>/dev/null || \
  { git rev-parse --verify -q HEAD >/dev/null 2>&1 && \
    echo "v0.0.0-$$(git rev-list --count HEAD)-g$$(git rev-parse --short HEAD)$$(git diff-index --quiet HEAD -- || echo -dirty)"; })
VERSION      ?= $(if $(GIT_DESCRIBE),$(shell echo '$(GIT_DESCRIBE)' | sed -E \
  's/^v//; s/^(.+)-0-g[0-9a-f]+$$/\1/; s/^(.+)-([0-9]+)-g([0-9a-f]+)$$/\1+dev.\2.g\3/; s/^(.+)-([0-9]+)-g([0-9a-f]+)-dirty$$/\1+dev.\2.g\3.dirty/'),0.0.0+dev.unknown)

LDFLAGS := -X $(MODULE)/internal/version.version=$(VERSION)

GINKGO := go tool ginkgo -r -p --randomize-all --fail-on-pending --keep-going

COMPOSE := podman compose
# Silence podman's "executing external compose provider" banner.
export PODMAN_COMPOSE_WARNING_LOGS = false

.PHONY: all build version cover mutate mutate-store mutate-sql test test-unit test-db test-acceptance test-policy lint check-generated generate clean package release image-tags db-up db-down db-reset db-migrate

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
# is the everyday case. PKG names the package; internal/domain by default.
#
# gremlins v0.6.0 keys the lines a diff touches by the repository-relative
# paths git prints, but looks each mutant up by its package-relative name,
# so from the module root every mutant is skipped and the run reports
# nothing runnable. gremlins therefore runs from inside the package, with
# git told to print paths relative to it, and is given the package's
# absolute path, because a relative one fails its module lookup. Drop the
# workaround when gremlins matches the two.
PROPERTY_CHECKS ?= 100
PKG ?= internal/domain
MUTATE_FLAGS := $(if $(DIFF),--diff $(DIFF),)
gremlins_in = set -a && . ./.env && set +a && cd $(1) && \
	  SKIFT_TEST_POSTGRES_URL="$$DATABASE_URL" SKIFT_PROPERTY_CHECKS=$(PROPERTY_CHECKS) GOFLAGS=-count=1 \
	  GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.relative GIT_CONFIG_VALUE_0=true $(2) \
	  gremlins unleash $(MUTATE_FLAGS) $(3) "$$PWD"
mutate: db-up
	$(call gremlins_in,$(PKG))

# The store holds the SQL the domain composes its rules from, and the
# domain and server specs are what exercise it, so a store mutant is
# judged against every suite in the module rather than the store's own.
# SKIFT_MUTATING leaves out the two suites that would cost minutes per
# mutant and never reach the store: the development-environment specs at
# the root, which run make lint in copies of the tree, and the acceptance
# scenarios. Generated query code is not mutated; make mutate-sql judges
# the SQL itself.
mutate-store: db-up
	$(call gremlins_in,internal/store,SKIFT_MUTATING=1,--integration --coverpkg $(MODULE)/internal/store/... --exclude-files '\.gen\.go$$')

# gremlins mutates Go operators, and the store's rules are SQL. mutate-sql
# breaks each rule in internal/tools/mutatesql/mutants.go in turn, runs the
# store, domain and server specs against it, and fails unless a spec
# notices every one. Like mutate, it works in the development database.
mutate-sql: db-up
	set -a && . ./.env && set +a && \
	  SKIFT_TEST_POSTGRES_URL="$$DATABASE_URL" go run ./internal/tools/mutatesql

# Every check tool is a step in hk.pkl; hk runs them concurrently.
# HK_FLAGS passes options through, such as --no-fail-fast. hk reports on
# stderr; lint's findings go to stdout, where lint_test.go reads them.
lint:
	hk validate
	hk check --all $(HK_FLAGS) 2>&1

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

# The commit's time, in seconds, which every timestamp in an archive and
# an rpm takes. Empty outside a git checkout.
COMMIT_EPOCH := $(shell git log -1 --format=%ct 2>/dev/null)

# rpms runs nfpm over packaging/nfpm.yaml for each GOARCH in $(1) and
# writes the packages into $(2). Each packs dist/skift-linux-<arch>,
# whichever target put it there, and dist/packaging/preinstall.sh, which
# a target that calls it names as a prerequisite.
define rpms
mkdir -p $(2)
for arch in $(1); do \
  GOARCH=$$arch VERSION='$(VERSION)' $(if $(COMMIT_EPOCH),SOURCE_DATE_EPOCH=$(COMMIT_EPOCH)) \
    nfpm package --config packaging/nfpm.yaml --packager rpm --target $(2)/ || exit 1; \
done
endef

# The rpm, for each architecture in PACKAGE_ARCHES: a cross-compiled skift
# and the units, fragments and scriptlets under contrib/ and packaging/. A
# release's rpms are the same package around the binaries in its archives;
# see release.
PACKAGE_ARCHES ?= amd64 arm64
# Where the finished rpms go. stead passes its channel directory; staging
# stays under dist/ regardless.
DESTDIR ?= dist

package: $(foreach arch,$(PACKAGE_ARCHES),dist/skift-linux-$(arch)) dist/packaging/preinstall.sh
	$(call rpms,$(PACKAGE_ARCHES),$(DESTDIR))

dist/skift-linux-%: FORCE
	CGO_ENABLED=0 GOOS=linux GOARCH=$* go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/skift

# The newest migration in the tree, which every binary built from it
# carries: the greatest number before the underscore in goose's
# NNNNN_name.sql files under internal/store/migrations.
SCHEMA_VERSION := $(shell ls internal/store/migrations 2>/dev/null | \
  sed -n 's/^0*\([0-9][0-9]*\)_.*\.sql$$/\1/p' | sort -n | tail -n 1)

# The rpm's pre-install scriptlet with this package's version and schema
# written in, since nfpm takes a scriptlet as a file. FORCE, because
# VERSION is not a file make can compare.
dist/packaging/preinstall.sh: packaging/scripts/preinstall.sh FORCE
	@test -n '$(SCHEMA_VERSION)' || { echo "$@: no migration found under internal/store/migrations" >&2; exit 1; }
	@mkdir -p $(@D)
	sed -e 's/@VERSION@/$(VERSION)/g' -e 's/@SCHEMA_VERSION@/$(SCHEMA_VERSION)/g' $< > $@.tmp
	@! grep -n '@[A-Z_]*@' $@.tmp || { echo "$@: a placeholder is left unfilled" >&2; rm -f $@.tmp; exit 1; }
	mv $@.tmp $@

FORCE:

# What a release uploads, in dist/release/: for each platform an archive
# and its SBOM, an rpm for each linux architecture, and SHA256SUMS over
# them all. goreleaser builds the archives from .goreleaser.yaml, always
# as a snapshot: it publishes nothing and takes its version from here, so
# a rehearsal and a release are the same build. The linux binaries are
# then lifted out of their archives to where packaging/Containerfile and
# the rpm read them, so the image and the rpms hold the very bytes a
# release's archives do.
#
# Every timestamp in an archive and an rpm is the commit's, passed in each
# spelling goreleaser's templates and nfpm take, so one commit always
# builds the same bytes.
RELEASE_DIR := dist/release
# sha256sum's output, from whichever of the two tools this machine has.
SHA256SUM := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256)

release: dist/packaging/preinstall.sh
	rm -rf $(RELEASE_DIR)
	test -n '$(COMMIT_EPOCH)' || { echo "release: no commit to take the time from" >&2; exit 1; }
	date=$$(TZ=UTC0 git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd) && \
	VERSION='$(VERSION)' SOURCE_DATE_EPOCH=$(COMMIT_EPOCH) COMMIT_DATE=$$date goreleaser release --snapshot --clean
	mkdir -p $(RELEASE_DIR)
	cp dist/goreleaser/skift_$(VERSION)_*.tar.gz dist/goreleaser/skift_$(VERSION)_*.cdx.json dist/goreleaser/SHA256SUMS $(RELEASE_DIR)/
	for arch in amd64 arm64; do \
	  tar -xzOf $(RELEASE_DIR)/skift_$(VERSION)_linux_$$arch.tar.gz skift > dist/skift-linux-$$arch && \
	  chmod 0755 dist/skift-linux-$$arch || exit 1; \
	done
	$(call rpms,amd64 arm64,$(RELEASE_DIR))
	cd $(RELEASE_DIR) && $(SHA256SUM) skift-*.rpm >> SHA256SUMS

# The compose file a release carries: contrib/docker-compose.yaml with the
# skift service's image line naming IMAGE, the released image as
# <version>@sha256:<digest>, and every other line as it is. IMAGE is known
# only once the image has been pushed, so this is not part of `make
# release`, which also empties the directory: render it afterwards.
# FORCE, because a file left by an earlier IMAGE is not this one's.
$(RELEASE_DIR)/docker-compose.yaml: IMAGE ?=
$(RELEASE_DIR)/docker-compose.yaml: contrib/docker-compose.yaml FORCE
	@printf '%s\n' '$(IMAGE)' | \
	  grep -Eqx 'ghcr\.io/steading-ai/skift:[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?@sha256:[0-9a-f]{64}' || \
	  { echo "$@: IMAGE must be ghcr.io/steading-ai/skift:<version>@sha256:<64 hex>, not '$(IMAGE)'" >&2; exit 1; }
	@mkdir -p $(@D)
	@awk -v image='$(IMAGE)' ' \
	  /^ *image: *[$$][{]SKIFT_IMAGE/ { sub(/image:.*/, "image: " image); pinned++ } \
	  { print } \
	  END { if (pinned != 1) { print "$@: $< has " (pinned + 0) " image lines reading SKIFT_IMAGE, not one" > "/dev/stderr"; exit 1 } }' \
	  $< > $@ || { rm -f $@; exit 1; }

# The image tags a release of VERSION sets, one per line, given the v*
# tags in the repository. Always the version itself, which never moves,
# and edge unless a later version is tagged, so edge only moves forward.
# A release is a plain X.Y.Z. Any other version, such as one with a
# pre-release part or the build metadata every development build has, is
# not a release: the target fails and prints no tag. stable, latest and
# X.Y move on promotion, which no release does.
# docs/adrs/017-release-channels-and-packages.md, decision 3.
#
# The tags are read from git, so they must have been fetched: without
# them every release would move edge.
image-tags:
	@tags=$$(git tag --list 'v*') || exit 1; \
	printf '%s\n' "$$tags" | awk -v version='$(VERSION)' ' \
	  BEGIN { \
	    release = version ~ /^[0-9]+\.[0-9]+\.[0-9]+$$/; \
	    split(version, v, "."); major = v[1] + 0; minor = v[2] + 0; patch = v[3] + 0; \
	    if (release) print version \
	  } \
	  release && /^v[0-9]+\.[0-9]+\.[0-9]+$$/ { \
	    split(substr($$0, 2), t, "."); a = t[1] + 0; b = t[2] + 0; c = t[3] + 0; \
	    if (a > major || (a == major && (b > minor || (b == minor && c > patch)))) later = 1 \
	  } \
	  END { \
	    if (!release) { \
	      print "image-tags: " version " is not a release version, which is a plain X.Y.Z" > "/dev/stderr"; exit 1 \
	    } else if (!later) print "edge" \
	  }'

# Development database. See compose.yaml and docs/developing.md.

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
