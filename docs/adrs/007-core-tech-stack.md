---
status: accepted
date: 2026-09-05
---

# Core tech stack: Go, buf, sqlc, goose, pgx, Ginkgo

## Context and Problem Statement

Dagger is a greenfield Go service and CLI backed by PostgreSQL, serving a protobuf contract over Connect. Which libraries and tools form the foundation, and where does the standard library suffice?

## Decision Drivers

- The proto contract is the domain model ([ADR-002](002-connect-rpc-is-the-only-door.md)), so code generation from proto and from SQL must both be first class
- The interesting tests are SQL: readiness, cycle detection, and claims under concurrency
- One binary serves three faces: `dg` the CLI, `dg mcp` the stdio MCP server, `dg serve` the service
- Package boundaries must be enforceable, since the store must never be reachable except through the domain layer
- Embrace systemd: logging goes to the journal, the service and its reaper are units

## Considered Options

1. Generated everywhere: buf for proto, sqlc for SQL
2. An ORM such as GORM or Ent
3. Hand-written SQL with `database/sql`

## Decision Outcome

Chosen option: **generated everywhere**, because both of dagger's contracts (the wire and the schema) are declarative artifacts that should produce types rather than be restated in Go by hand.

| Concern | Choice |
|---|---|
| Language | Go 1.25+ |
| Proto toolchain | buf, with `connectrpc.com/connect` |
| Database driver | `github.com/jackc/pgx/v5` |
| Query generation | sqlc, reading the goose migrations directly |
| Migrations | goose |
| Policy | Open Policy Agent, embedded via the Rego Go package ([ADR-004](004-embedded-opa-authorization.md)) |
| CLI | cobra and pflag |
| Configuration | viper |
| Logging | `log/slog` with a journald handler |
| Tests | Ginkgo and Gomega |
| Integration tests | testcontainers-go against real PostgreSQL |
| Mocks | `go.uber.org/mock` |
| Property tests | `pgregory.net/rapid` |
| Lint | golangci-lint, including depguard and forbidigo |
| Hooks | hk |
| Tool versions | mise |

Two lint rules carry design decisions rather than style:

- **depguard** enforces that only the domain layer imports the store package. Handlers reach the database through the domain, never around it.
- **forbidigo** keeps `testing` out of everything but `*_suite_test.go`, so specs stay Ginkgo.

`rapid` earns its place on one property in particular: after any random sequence of patches, the graph is acyclic and the `ready` view equals a from-scratch recomputation. That invariant is what the whole system rests on.

### Consequences

- **Good**: The proto and the schema each have exactly one source of truth, and drift becomes a compile error.
- **Good**: sqlc reads goose migrations directly, so the two compose without a separate schema file.
- **Good**: testcontainers means concurrency tests run against real row locking rather than a fake.
- **Bad**: Two code generators in the build, and generated code in the repository.
- **Bad**: Wire types and storage types are separate, with a mapping layer between them. This is the deliberate cost of [ADR-002](002-connect-rpc-is-the-only-door.md).
- **Neutral**: No async job queue in v1. The only background work is lease expiry, which is a systemd timer invoking the binary.

## Rejected Options

### An ORM (GORM, Ent)

- **Good**: Less boilerplate for simple CRUD.
- **Bad**: Dagger's central queries are recursive CTEs, anti-joins, and `FOR UPDATE SKIP LOCKED`. An ORM adds a layer to fight rather than a layer to use.
- **Bad**: Encourages the storage model to leak outward, which is the thing [ADR-002](002-connect-rpc-is-the-only-door.md) exists to prevent.

### Hand-written SQL with `database/sql`

- **Good**: No generator.
- **Bad**: Every column change becomes a manual scan-target audit, and pgx's native interface is better than `database/sql` for PostgreSQL specifics anyway.

## More Information

Revisit the no-queue decision if background work grows beyond lease expiry. The retrieval service, which does need a queue, is a separate program.
