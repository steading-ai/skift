---
status: accepted
date: 2026-11-25
---

# Background jobs run on River, and operations live in an admin package

## Context and Problem Statement

[ADR-007](007-core-tech-stack.md) chose no async job queue in v1: "The only background work is lease expiry, which is a systemd timer invoking the binary." It named its own trigger: revisit the decision if background work grows beyond lease expiry. It has. Event partitions must be kept ahead of the clock, or every write fails once the last one is behind, and more work of that kind is expected.

The timer has a gap of its own. Only the rpm installs the reaper's systemd timer. The compose file runs `skift serve` and nothing else, and a launchd install runs `serve` alone too, so on those deployments stale leases are never expired.

The domain layer has also become three packages in one. Because depguard lets only the domain import the store, everything that needs the database landed there, whether or not it is a rule of the graph: the graph's rules and use cases, application services, and operational plumbing such as applying migrations, opening the database, bootstrapping the first principal, and reaping leases. Its package comment had stopped being true.

Where does skift's own background work run, so that every deployment gets it exactly once, and where do the operations that are not graph rules live?

## Decision Drivers

- Background work must run in every deployment shape: rpm, containers and macOS, not only where systemd timers exist
- Each periodic job must run once, however many `serve` processes share a database
- PostgreSQL is a commitment, and one migration system is easier to reason about than two ([ADR-013](013-postgresql-only-through-the-store.md))
- Each package should be describable in one sentence, and lint should enforce the boundaries rather than review
- More background work is expected, though its shape is not yet known

## Considered Options

1. River, running periodic jobs inside `serve`, with operations in a new `admin` package
2. A ticker inside `serve`, guarded by a PostgreSQL advisory lock
3. Keep the systemd timer and add equivalents for each deployment shape

## Decision Outcome

Chosen option: **River, running periodic jobs inside `serve`, with operations in a new `admin` package**, because it closes the deployment gap and the multi-process race with one mechanism, at a cost that is small next to writing scheduling and leader election by hand.

**River runs skift's own background work.** [River](https://github.com/riverqueue/river) is a job queue on PostgreSQL, built on pgx. Its periodic jobs run inside `serve`, and its leader election runs each one once across every `serve` process sharing the database. It replaces the reaper timer: lease expiry and partition upkeep become periodic jobs. River runs skift's maintenance; it does not run work from the graph, which agents claim through leases as before.

**River's schema is a goose migration.** `river migrate-get` prints River's schema as SQL. That SQL is embedded as goose migrations, with a comment naming the River version and the command that produced it, so there is one migration system, read by sqlc and linted like any other migration. Upgrading River means importing the migrations its new version adds.

**Three packages, each with one purpose.**

| Package | Purpose |
|---|---|
| `internal/store` | Reaches PostgreSQL: every SQL statement, the connection pool, `LISTEN`, and River's client. |
| `internal/domain` | States the graph's rules and the use cases built on them. |
| `internal/admin` | Operates a deployment: migrations, bootstrap, opening the database, and the background jobs with their schedule. |

The store builds River's client from its own pool with River's pgx driver, and exposes starting, stopping and enqueueing. The admin package defines the jobs and their schedule. A job that applies a graph rule, such as expiring a lease, calls the domain. A job that keeps storage in shape, such as creating partitions, calls the store.

**Allowed dependencies, enforced by depguard.**

- The store may be imported by `internal/domain` and `internal/admin`. ADR-007's intent was that handlers never reach the database around the domain; `admin` is not a handler.
- `github.com/jackc/pgx/v5` and River's drivers (`riverdriver`, `riverpgxv5`) are allowed only in `internal/store`.
- River's core package is allowed only in `internal/store` and `internal/admin`. A worker and a periodic job carry no PostgreSQL types, so admin defines them without importing pgx.
- The domain imports neither River nor anything that opens a connection.

**The CLI follows the packages.** Operating a deployment lives under `skift admin`: `skift admin migrate`, `skift admin jobs list`, and `skift admin jobs run <job> [--wait]`, which enqueues the job into River so that a manual run takes the same path as a scheduled one. The root `migrate` and `reap` commands go away. Before 1.0 a minor release may change the CLI without a deprecation period, so their removal is a line in the release notes.

### Consequences

- **Good**: Every deployment shape gets background work, because it runs in the process every deployment already runs.
- **Good**: A periodic job runs once however many `serve` processes there are, with no locking code of skift's own.
- **Good**: Each package has one sentence of purpose, and the domain holds only the graph's rules again.
- **Good**: Retries, scheduled jobs and durable work are available when the next kind of background work needs them.
- **Bad**: A new dependency, with tables of its own in skift's database.
- **Bad**: River's schema changes become skift's: upgrading River means importing its new migrations, and they fall under the upgrade promise like any other.
- **Bad**: When `serve` is not running, nothing runs the jobs. `skift admin jobs run --wait` says so rather than doing the work in the CLI process.
- **Neutral**: The store now has two kinds of client: the query set and River's.

## Rejected Options

### A ticker inside `serve`, guarded by an advisory lock

Each `serve` process wakes on an interval and takes `pg_try_advisory_lock` before doing the work.

- **Good**: About fifty lines, no new tables, and no dependency.
- **Bad**: Scheduling, leader election and failure handling become skift's own code to write and maintain, and it would be outgrown the first time a job needs a retry or durable scheduling.

### Keep the systemd timer and add equivalents for each deployment shape

A reaper service in the compose file, a second launchd job on macOS, and the systemd timer as before.

- **Good**: No change to `serve`, and each job stays a separate process.
- **Bad**: Every deployment shape has to remember every job, which is how compose and macOS came to have none.
- **Bad**: Every new job has to make overlapping runs safe on its own, as lease expiry does today.

## More Information

This supersedes the no-queue clause of [ADR-007](007-core-tech-stack.md), and builds on [ADR-013](013-postgresql-only-through-the-store.md), which made the store the only package that reaches PostgreSQL. Reactive work stays outside skift: notifications and other consumers are separate programs reading the [change stream](../domain-model.md#change-stream), as [the architecture](../architecture.md#consumers) describes.
