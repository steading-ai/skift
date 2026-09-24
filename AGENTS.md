# AGENTS.md

## What this repo is

Skift is an agent-first project management system built on directed acyclic graphs. A project is a graph of work; when a node's dependencies are satisfied it is ready, and ready work can be claimed and run in parallel. Skift stores the plan. It does not orchestrate, sandbox, or supervise.

- **Design is frontloaded and authoritative.** `docs/` settles the decisions; code follows it, not the other way round.
- **The repo builds itself.** Its own backlog lives in a running skift service, and `harness/loop.py` dispatches agents against it.
- **Success** is an orchestrator completing a dependency chain end to end, with every step in the event log.

## If you were dispatched to work a node

This is the common case, and your work order arrived with you: the node's body plus the result of every done node that blocks it. Read more of the graph with `skift` when you need it:

```bash
skift node show '#18'   # the node in full
skift inputs '#18'      # what its finished predecessors handed over
skift explain '#18'     # why something is not ready
```

The work order names the documents that govern it. Read them before writing code; they are the authority when they disagree with existing code.

Your contract is the node's **Acceptance criteria** and **Scenarios**. Every criterion must hold, and every Gherkin scenario must exist as an executable Ginkgo spec whose assertions match its Then clauses. A reviewer checks exactly this and nothing else.

**Do not modify** `harness/`, `scripts/`, `.claude/`, or `backlog.toml`. Those are the machinery running you and the record it grew from; writes there are reverted and returned as a review failure. `docs/` and `plugin/` are not protected — work orders name them as deliverables. Do not commit; the harness owns history.

## Repo map

```
cmd/skift/       binary entrypoint; one binary, three faces (CLI, MCP server, service)
internal/        application code; only internal/domain may import internal/store
proto/, gen/     the wire contract and its generated Go. The proto is the domain model
policy/          the default authorization bundle, in Rego, with its own spec suite
docs/            the design. Authoritative
  adrs/          decisions and their rationale
  plans/         the current sprint, in prose
plugin/          the skift Claude Code plugin: the skill agents read, and the MCP server
tests/acceptance end-to-end scenarios against the built binary and a real database
harness/         the agent loop that works one epic of skift's own backlog
scripts/ready.py a paper test of the readiness rules, from before there was a schema
backlog.toml     skift's first backlog, kept as a record. Nothing reads it
compose.yaml     PostgreSQL for local development
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
make mutate             # mutation testing over internal/domain; takes hours, read the survivors
make lint               # go vet, golangci-lint, buf, opa, and a generated-code check
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
- **What is planned and deliberately deferred** — `docs/roadmap.md`, then the current sprint in `docs/plans/`.
- **How the loop that dispatched you works** — `harness/README.md`.

## Running the build loop

The loop works one epic: a grouping node whose open children it labels and then claims.

```bash
harness/.venv/bin/python harness/loop.py --epic '#1' --dry-run   # what is ready under the epic
harness/.venv/bin/python harness/loop.py --epic '#1'             # run until the epic drains
harness/.venv/bin/python harness/loop.py --report                # per-node tokens and cost
```

An implementer builds a node, a reviewer judges it against its acceptance criteria and scenarios only, and a pass is committed and pushed. See `harness/README.md`.
