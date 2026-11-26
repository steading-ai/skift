---
status: accepted
date: 2026-11-26
---

# Unattended upgrades snapshot first, downgrade within a compatibility floor, and never stop

## Context and Problem Statement

[ADR-015](015-upgrades-and-migrations.md) promises that any release upgrades directly from any earlier one. As first decided, it allowed one exception, a required stop: a release every operator must install and start before upgrading further, announced in its release notes. Its way back is a restore of a backup the operator took before upgrading, and it keeps an older binary safe by having every command refuse a schema newer than the binary (`SchemaAheadError` in `internal/store/migrations`). It says nothing yet takes that backup for the operator, and asks to be revisited if skift starts taking its own.

Upgrades are about to be unattended. `dnf-automatic` installs each release from `pkgs.stead.ing` as soon as it reaches the machine's channel (ADR-017, on release channels and packages), and the rpm's postremove step restarts the service onto the new binary, which migrates as it starts. Releases ship several times a week, or daily, and nobody is at the keyboard when one installs. An operator who never ran a backup has nothing to restore, an operator who did can only go back by restoring it, and dnf cannot be told to stop at a particular release.

How does an upgrade that no one watches stay uneventful: what happens to the data before a migration runs, how does an operator get back from a release that misbehaves after it has migrated, and what replaces required stops?

## Decision Drivers

- Upgrades run unattended, from `dnf-automatic`, several times a week or daily, and must be completely uneventful
- dnf always installs the newest version it can see; it cannot hold a machine at a particular release, and a machine switched off for a month jumps straight to the newest one
- An upgrade that cannot be made safe should leave the older release running, not the service down
- An operator needs a way back from a broken release that does not depend on having remembered to take a backup
- A snapshot must work wherever skift runs, against a local or a remote database, with nothing installed beside skift
- Each safeguard is held by the cheapest mechanism that cannot be forgotten, as in ADR-015
- Skift manages its own database, so a fix to the schema ships as a migration or a background job, never as a step an operator runs by hand

## Considered Options

1. Snapshot in the package's pre-install step, written by skift over pgx, with a compatibility floor that rises only to a release proven on `stable`, and no required stops
2. Keep required stops, announced in release notes
3. Snapshot when `skift serve` starts
4. Snapshot with `pg_dump`
5. A floor of one release back
6. Never raise the floor

## Decision Outcome

Chosen option: **snapshot in the package's pre-install step, written by skift over pgx, with a compatibility floor that rises only to a release proven on `stable`, and no required stops**, because it is the only combination in which an unattended upgrade either completes or leaves the older release running, and in which an operator who upgrades at least weekly can go back with a plain `dnf downgrade`.

**No required stops.** No release is one an operator must install and start before upgrading further. ADR-015's promise stands without its exception: any release upgrades directly from any earlier one. Work that would have needed a stop becomes a migration that applies in one run from every released schema, or a background job the new binary carries and runs inside `serve` ([ADR-016](016-background-jobs-and-admin.md)). A migration author never introduces a required stop, and no release notes announce one.

**Snapshot before migrating, in the package's pre-install step.** On an upgrade, the new package's pre-install script runs the installed, older binary's `skift admin snapshot --if-behind <schema>`, where the schema is the new release's newest migration. The command skips when the database is already at that schema, so an upgrade that adds no migration does nothing extra. If the snapshot fails, the script fails, the install aborts, and the older release keeps running; `dnf-automatic` reports the failure and tries again on its next run. This is GitLab Omnibus's model, which backs up the database before an upgrade and aborts the upgrade when the backup fails. The first upgrade that takes a snapshot is the one from the first release that has the command.

**skift writes the snapshot itself, over pgx.** `skift admin snapshot` copies every table out within one consistent transaction, to `/var/lib/skift/backups/`, labelled with the schema version it was taken at, and keeps the last few. `skift admin restore` rebuilds the snapshot's schema from the binary's embedded migrations, which never change once shipped, then copies the rows back. Only an operator runs `restore`; nothing restores automatically, because a restore discards every write made since the snapshot. Scheduled backups remain part of the M4 milestone in the [roadmap](../roadmap.md).

**A compatibility floor replaces refusing every newer schema.** Each schema has a floor: the oldest release that can serve it. The floor is stored in the database, as the lowest migration version a binary must carry, because an older binary cannot read migrations it does not carry. A release serves any schema whose floor it meets, even one newer than itself, and refuses one whose floor it does not, naming both. A migration that only adds leaves the floor alone. A migration that removes or changes something raises the floor, and may raise it only to a release that has been on `stable` for at least seven days. Anyone who upgrades at least weekly can therefore `dnf downgrade` to the release they ran before; anyone else restores the pre-install snapshot.

**Where safe downgrades start.** v0.4.0 refuses any schema newer than itself, so downgrading to it works only if the release after it adds no migration. Safe downgrades start from the first release that has the floor. An operator who downgrades also holds the version with `dnf versionlock`, or `dnf-automatic` upgrades it again on its next run.

**The operator watches upgrades.** skift's part is to fail clearly: in `dnf-automatic`'s report, in systemd, and in `skift doctor`. It does not roll back on its own and does not page anyone.

**The package never changes `/etc/skift/env` after installing it.** An upgrade therefore never leaves `env.rpmnew` for an operator to reconcile. Every default lives in the binary, and the file holds only what the operator set.

**What enforces it.** Each safeguard is held by the cheapest mechanism that cannot be forgotten. These mechanisms are built by the work that follows this decision; until each lands, ADR-015's corresponding safeguard stays in force.

| Safeguard | Mechanism |
|---|---|
| No release is a required stop: every migration applies in one run from every released schema, with data | Test: ADR-015's upgrade spec, unchanged |
| An upgrade that migrates has a snapshot of the data it started from, or does not happen | Packaging: the new rpm's pre-install script runs the installed binary's `skift admin snapshot --if-behind`, and fails the install when the snapshot fails |
| A snapshot is consistent and can be restored with nothing but skift | Code: `skift admin snapshot` copies every table in one transaction over pgx, labelled with its schema; `skift admin restore` rebuilds that schema from the embedded migrations and copies the rows back |
| An older binary never serves a schema it cannot | Code: the floor, stored in the database as the lowest migration version a binary must carry; every command that opens the database refuses a schema whose floor is above its newest migration |
| A migration that leaves the floor alone leaves every release at or above it able to serve the new schema | Test: a downgrade spec that runs each released binary at or above the floor against the current schema, seeded with rows |
| The floor rises only to a release that has been on `stable` for seven days | Linter: a floor policy check, as an hk step over each new migration, that refuses one raising the floor to a release not yet seven days on `stable`, from the promotion history `pkgs.stead.ing` publishes |
| No upgrade leaves `env.rpmnew` | Packaging: the package creates `/etc/skift/env` on first install only and never ships content there that can change; every default lives in the binary |
| Whether a migration removes or changes something, and so must raise the floor | Review, backed by the downgrade spec, which fails when such a migration leaves the floor where it was |

### Consequences

- **Good**: An unattended upgrade either completes or leaves the older release running; a failed snapshot never leaves the service down.
- **Good**: Every upgrade that migrates has a snapshot of the data it started from, without the operator remembering to take one, and with no `pg_dump` to match to the server.
- **Good**: An operator who upgrades at least weekly goes back from a broken release with `dnf downgrade`, keeping every write made since.
- **Good**: A machine that was off for a month upgrades in one step like any other, because no release is a stop.
- **Bad**: Work too long for a migration inside `serve`'s start-up has to be written as a background job that runs alongside the new release, rather than split across releases.
- **Bad**: The chain of migrations only grows, since no required stop ever lets it be squashed.
- **Bad**: Removing or changing anything in the schema waits until the release that stops using it has been on `stable` for seven days, so dead columns and tables outlive the code that used them by at least that long.
- **Bad**: Snapshots take disk space beside the service, and a host short of it fails the upgrade instead of completing it.
- **Neutral**: Downgrading to v0.4.0 or earlier still needs the pre-install snapshot or the operator's own backup, as ADR-015 described.
- **Neutral**: Watching upgrades is the operator's job; skift fails clearly and does nothing more.

## Rejected Options

### Keep required stops, announced in release notes

ADR-015's exception: a migration that cannot apply from every earlier release makes its release a stop, which operators install and start before upgrading further.

- **Good**: A long or multi-step data migration could be split across releases, each applying from the one before.
- **Bad**: dnf always installs the newest version, so it cannot enforce a stop, and a machine switched off for a month jumps straight past one. An unattended upgrade across a stop would fail, which is exactly the eventful upgrade this decision rules out.

### Snapshot when `skift serve` starts

`serve` takes the snapshot itself before applying pending migrations.

- **Good**: It needs nothing from the package, so it would cover every deployment shape, containers included.
- **Bad**: By the time `serve` starts, the rpm has already replaced the binary. A failed snapshot would leave the service down instead of leaving the older release running.

### Snapshot with `pg_dump`

The pre-install step runs `pg_dump` against the database.

- **Good**: A standard format, restorable with standard tools.
- **Bad**: `pg_dump`'s version must match the server's, the host may not have it installed, and a snapshot has to work against a remote database from wherever skift runs.

### A floor of one release back

Every schema is always servable by the release before the one that migrated it.

- **Good**: A simple rule, checkable from the release history alone.
- **Bad**: With daily releases and automatic promotion, `stable` jumps several versions at once, so the previous version is not what a `stable` user last ran, and `dnf downgrade` to the release they actually ran would fail.

### Never raise the floor

Every migration only adds, so every release since the floor began can serve every later schema.

- **Good**: Any downgrade within that range always works.
- **Bad**: Nothing is ever removed, so dead schema accumulates forever.

## More Information

[ADR-015](015-upgrades-and-migrations.md) remains the decision for how migrations are written and checked: squawk, the lock timeout, the start-up budget, the upgrade spec and the check against the newest release tag. This decision replaces its required stops, its rule that a downgrade is a restore, its operator's backup step, and its refusal of every newer schema. ADR-017 settles the release channels, the package repository and its signing, which make upgrades unattended. Revisit the pre-install snapshot if dnf turns out to abort more than the one package when a pre-install step fails, and the seven days if the rules for promotion to `stable` change.
