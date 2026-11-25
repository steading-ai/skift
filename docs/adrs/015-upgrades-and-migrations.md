---
status: accepted
date: 2026-11-25
---

# Upgrades go forward from any release, and tools hold migrations to it

## Context and Problem Statement

Every release from v0.1.0 to v0.3.0 carries the same twenty migrations, so no migration has yet run against a live, populated database. v0.4.0 ships the first. The binary embeds its migrations and `skift serve` applies any that are pending before it reports ready, so an upgrade is: install the new package, restart, and the service migrates. Nothing has said which upgrades must work, what an operator does to go back, or what keeps a migration from locking a busy table or failing on rows that a fresh install never had.

The history shows how easy that is to get wrong. `00014` adds `policy_version text NOT NULL` to `events` with no default, which fails on any table holding a row. `00016` adds `tx_id` with a default of `pg_current_xact_id()`; the function is stable, so the table is not rewritten, but every existing event would be stamped with the migrating transaction's id. Several migrations build indexes and constraints on existing tables without `CONCURRENTLY` or `NOT VALID`, which blocks writes for the length of the build. All of them ran only against empty databases.

What does skift promise an operator about upgrading and downgrading, and how is each migration held to that promise when agents write most of them?

## Decision Drivers

- An operator should be able to upgrade from whatever they run today to the newest release in one step
- Most migrations are written by agents, so a rule held only by a written style guide erodes; a rule a tool checks does not
- A migration runs while the service starts, inside systemd's start timeout, against a table other processes may be using
- Skift manages its own database, so a fix to the schema ships as a migration, never as a step an operator runs by hand
- Fit for one server, shaped for a thousand: build the machinery a single serving process needs, and name the triggers for the rest

## Considered Options

1. Upgrade from any release, held by a linter, code, and tests
2. Upgrade one minor release at a time, as GitLab requires for zero-downtime upgrades
3. A written migration style guide, held by review

## Decision Outcome

Chosen option: **upgrade from any release, held by a linter, code, and tests**, because it is the simplest promise for an operator and the only one whose safeguards do not depend on someone remembering them.

**The promise.** Any released version of skift upgrades directly from any earlier released version, by installing the new release and starting it. A migration added to skift must therefore apply cleanly, in one run, to the schema of every earlier release, with that release's data in every table. A migration that cannot, because it depends on a previous migration having run against a live database first, or because it needs longer than the service's start-up allows, requires a required stop: a release that operators must install and start before upgrading further. The maintainer who cuts the release decides whether to take a required stop, and its release notes announce it. An agent writing such a migration stops and says so; it does not introduce the stop itself.

**The budget.** Migrations run inside `skift serve`'s start-up, which systemd bounds with its default start timeout of 90 seconds. Every pending migration, with its lock retries, must finish well inside that.

**Downgrade is a restore.** An operator backs up the database before upgrading. To go back, they stop the service, restore that backup, and install the older release. Each migration's down section is a development tool, proven by the migration specs, and never a production rollback: a down migration cannot recover data its up migration discarded.

**What enforces it.** Each safeguard is held by the cheapest mechanism that cannot be forgotten.

| Safeguard | Mechanism |
|---|---|
| No index built without `CONCURRENTLY` on an existing table; constraints added `NOT VALID` and validated separately; no `NOT NULL` column without a default; no volatile default; no type change, drop or rename that locks a table | Linter: squawk, as an hk step over each new migration, aware of goose's transaction and `NO TRANSACTION` annotation |
| Two processes never apply a migration at once | Code: goose's PostgreSQL session lock around every migration run |
| A migration waiting on a lock gives up rather than queueing every other query behind it | Code: `lock_timeout` on the migration connection, with a bounded retry of the whole run |
| An older binary never serves a schema newer than it knows | Code: every command that opens the database refuses a schema ahead of the binary |
| Every migration applies from every released schema, with data | Test: an upgrade spec that migrates from each release's schema, seeded with rows in every table, to the current one |
| A shipped migration never changes, and a new one sorts after every shipped one | Test: a check against the newest release tag |
| What a default means for existing rows; how large a backfill is; whether a migration relies on data only a running release wrote | Review |

The review row stays short on purpose. Anything that can move into a linter, code, or a test moves there.

**Adopt when needed.** Three practices wait for a trigger rather than landing now. Compatibility between consecutive releases, where a column is added in one release and its predecessor removed in the next, waits for more than one serving process or for zero-downtime upgrades; until then the old binary has stopped before the new schema exists. Batched backfills, each batch in its own transaction, wait for a backfill that would take more than a few seconds at the target scale. Building an index on the partitioned `events` table one partition at a time waits for the first new index on `events` once it holds real volume.

### Consequences

- **Good**: An operator's upgrade is one step from any release, and their way back is a backup they already took.
- **Good**: A lock-unsafe statement, a concurrent migration, a stale binary and a migration that fails on real data are each caught by a machine before a release ships.
- **Good**: The upgrade spec makes the promise check itself: a migration that breaks it fails `make test`.
- **Bad**: Every migration has to work from the oldest release forward, so the chain of migrations only grows until a required stop lets it be squashed.
- **Bad**: Nothing yet takes the backup for the operator; it is a documented step until skift snapshots its own database.
- **Neutral**: A required stop is a person's decision, so an agent that needs one has to stop and ask.

## Rejected Options

### Upgrade one minor release at a time

GitLab requires one minor release at a time for zero-downtime upgrades, and fixes required stops at set minor versions.

- **Good**: Each migration only has to apply from the release before it, so long data migrations can be split across releases.
- **Bad**: Every operator who skips releases pays for it in sequential upgrades, for a guarantee skift does not yet need: one serving process restarts, so there is no window where old code meets a new schema.

### A written migration style guide, held by review

The rules above, written as prose for whoever writes the next migration.

- **Good**: No new tool to install or maintain.
- **Bad**: Each rule holds only as long as every author and reviewer remembers it, and the history above is what that produced.

## More Information

[ADR-013](013-postgresql-only-through-the-store.md) puts every migration and every SQL statement in the store. [Releasing](../releasing.md) is where a required stop is announced, and [deploying](../deploying.md#upgrade-it) has the operator's steps for upgrading and going back. Revisit the "adopt when needed" practices when skift runs more than one serving process, offers zero-downtime upgrades, or first needs a migration that takes minutes; revisit the backup step if skift starts taking its own.
