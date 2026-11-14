# AGENTS.md

## What this repo is

Skift is an agent-first project management system built on directed acyclic graphs. A project is a graph of work; when a node's dependencies are satisfied it is ready, and ready work can be claimed and run in parallel. Skift stores the plan. It does not orchestrate, sandbox, or supervise.

- **Design is frontloaded and authoritative.** `docs/` settles the decisions; code follows it, not the other way round.
- **The backlog lives in skift.** The work on this repository is a graph in a running skift service, and each node is a work order.
- **Success is what the roadmap's milestones name.** `docs/roadmap.md` defines each by what becomes possible, not by a date.

## If you are working a node

A node's body is the work order, and the result of every done node that blocks it is its input. Read them, and more of the graph when you need it, with `skift`:

```bash
skift node show '#18'   # the node in full
skift inputs '#18'      # what its finished predecessors handed over
skift explain '#18'     # why something is not ready
```

The work order names the documents that govern it. Read them before writing code; they are the authority when they disagree with existing code.

Your contract is the node's **Acceptance criteria** and **Scenarios**. Every criterion must hold, and every Gherkin scenario must exist as an executable Ginkgo spec whose assertions match its Then clauses.

## Repo map

```
cmd/skift/       binary entrypoint; one binary, three faces (CLI, MCP server, service)
internal/        application code; only internal/domain may import internal/store
proto/, gen/     the wire contract and its generated Go. The proto is the domain model
policy/          the default authorization bundle, in Rego, with its own spec suite
docs/            the design, which is authoritative, and the guides
  adrs/          decisions and their rationale
plugin/          the skift Claude Code plugin: the skills agents read, and the MCP server
.claude-plugin/  the marketplace manifest that offers the plugin from the repository root
packaging/       the rpm and the image: nfpm configuration, scriptlets, and the Containerfile
contrib/         what a deployment copies: the compose file, its settings template, and the systemd units
tests/acceptance end-to-end scenarios against the built binary and a real database
compose.yaml     PostgreSQL for local development
archive/         retired machinery and dated records. Unmaintained, not authoritative
```

## How to work here

Canonical commands live in `Makefile`; tool versions in `mise.toml`. Only podman is assumed to be installed.

```bash
mise install            # bootstrap every tool
make build              # ./skift
make test               # all specs
make test-unit          # specs needing no database
make test-db            # specs needing PostgreSQL, via testcontainers
make test-acceptance    # the end-to-end scenarios
make cover              # statement coverage of hand-written code, gated by coverage.baseline
make test-race          # all specs under the race detector
make fuzz               # the fuzz targets in *_fuzz_test.go, FUZZ_TIME each
make mutate             # mutation testing over internal/domain; slow, read the survivors. DIFF=main for changed lines only
make lint               # jactionlint over the workflows, go vet, golangci-lint, buf, opa, goreleaser check, hk validate, and a generated-code check
make release            # what a release uploads: archives, SBOMs and checksums, in dist/release/
make generate           # buf and sqlc codegen; commit the output
make db-up              # local PostgreSQL for interactive work
```

Before you finish, this must pass:

```bash
make lint && make test
```

Specs are Ginkgo and Gomega. Two lint rules carry design decisions rather than style: `depguard` stops anything but `internal/domain` importing `internal/store`, and `forbidigo` keeps `testing` out of everything but `*_suite_test.go`. If either fires, the fix is the code, not the config.

## Change hygiene

- Do the node you were given. If a dependency is missing, report it rather than building it.
- Add specs with the code. A criterion with no spec is not done.
- Follow the patterns already in the tree over your own preferences.
- No repo-wide reformatting, and no dependency the work order did not name.
- Never commit a secret. `.env` is ignored; `.env.example` is the template.
- Generated code is committed. Run `make generate` and include the result.

## Where to look first

- **What skift is and is not** — `docs/vision.md`, then `docs/core-principles.md`. The principles have teeth; violating one should feel wrong.
- **The model you are implementing** — `docs/domain-model.md`. Nodes, edges, readiness, holds, leases, patches, events. This is the single most useful file in the repo.
- **How the pieces fit** — `docs/architecture.md` for the request path, transports, and trust boundaries.
- **Why something is the way it is** — `docs/adrs/`. Check here before changing a decision.
- **What is planned and deliberately deferred** — `docs/roadmap.md`.
- **How to build it, run a development database and run the specs** — `docs/developing.md`.
- **How it is installed** — `docs/install.md` for a server in containers, the CLI and an agent; `docs/deploying.md` for the rpm under systemd; `docs/agent-skills.md` for the skills and the MCP server.
- **How it is checked and released** — `docs/ci.md` for the workflows, `docs/releasing.md` to cut a release, `docs/verifying-a-release.md` to check one.
- **What used to be here** — `archive/README.md`. The loop that built the first version, the first backlog, and dated plans and notes. Records, not guidance.
