# AGENTS.md

## What this repo is

Dagger is an agent-first project management system built on directed acyclic graphs. A project is a graph of work; when a node's dependencies are satisfied it is ready, and ready work can be claimed and run in parallel. Dagger stores the plan. It does not orchestrate, sandbox, or supervise.

- **Design is frontloaded and authoritative.** `docs/` settles the decisions; code follows it, not the other way round.
- **The repo builds itself.** Work is defined in `backlog.toml` and executed by agents through `harness/loop.py`.
- **Success** is an orchestrator completing a dependency chain end to end, with every step in the event log.

## If you were dispatched to work a backlog node

This is the common case. Read your work order first:

```bash
scripts/ready.py            # what is claimable now
scripts/ready.py --show 18  # the full work order for one node
```

The work order names the documents that govern it. Read them before writing code; they are the authority when they disagree with existing code.

Your contract is the node's **Acceptance criteria** and **Scenarios**. Every criterion must hold, and every Gherkin scenario must exist as an executable Ginkgo spec whose assertions match its Then clauses. A reviewer checks exactly this and nothing else.

**Do not modify** `backlog.toml`, `docs/`, `harness/`, or `scripts/`. Those are the specification and the machinery running you. If one is wrong, say so in your final message and carry on. Do not commit; the harness owns history.

## Repo map

```
cmd/dg/          binary entrypoint; one binary, three faces (CLI, MCP server, service)
internal/        application code; only internal/domain may import internal/store
docs/            the design. Authoritative
  adrs/          decisions and their rationale
harness/         the agent loop that builds this repo from backlog.toml
scripts/ready.py readiness rules over backlog.toml, mirroring dagger's own
backlog.toml     the work, expressed in dagger's domain model
compose.yaml     PostgreSQL for local development
```

## How to work here

Canonical commands live in `Makefile`; tool versions in `mise.toml`. Only podman is assumed to be installed.

```bash
mise install            # bootstrap every tool
make build              # ./dg
make test               # all specs
make test-unit          # specs needing no database
make test-db            # specs needing PostgreSQL, via testcontainers
make lint               # go vet + golangci-lint
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

- **What dagger is and is not** — `docs/vision.md`, then `docs/core-principles.md`. The principles have teeth; violating one should feel wrong.
- **The model you are implementing** — `docs/domain-model.md`. Nodes, edges, readiness, leases, patches, events. This is the single most useful file in the repo.
- **How the pieces fit** — `docs/architecture.md` for the request path, transports, and trust boundaries.
- **Why something is the way it is** — `docs/adrs/`. Check here before changing a decision.
- **What is planned and deliberately deferred** — `docs/roadmap.md`.
- **How the loop that dispatched you works** — `harness/README.md`.

## Running the build loop

```bash
mise exec -- python harness/loop.py --dry-run   # what would run next
mise exec -- python harness/loop.py             # run until the backlog drains
mise exec -- python harness/loop.py --report    # per-node token and cost tally
```

An implementer builds a node, a reviewer judges it against its acceptance criteria and scenarios only, and a pass is committed and pushed. See `harness/README.md`.
