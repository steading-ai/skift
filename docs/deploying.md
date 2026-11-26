# How to deploy skift from the rpm

This guide shows how to run the skift service under systemd from its rpm: build the package, install it, point it at a database, create the first caller, and start the units. To run the service in containers instead, and for the CLI and agent setup that follow either way, see [How to install skift](install.md).

You need:

- A checkout of this repository on a machine with [mise](https://mise.jdx.dev), to build the package. No package repository serves it.
- A host with systemd that installs rpm packages.
- A PostgreSQL database skift may create tables in. The binary carries its schema and can seed its own first caller, so the host needs no checkout, no migration tool, and no SQL client.

## Build the package

```sh
make package
```

`make package` cross-compiles `skift` and builds an rpm for `x86_64` and `aarch64` into `dist/`. To package a release, check out its tag first; `make version` prints the version the package will carry.

## Install it

On the host, from the checkout or from wherever you copied the rpm for its architecture:

```sh
sudo dnf install ./dist/skift-*.$(uname -m).rpm
```

The package installs the `skift` binary, the system units, and the `skift` user and group the service runs as, and starts nothing: the service needs a database, which is not the package's to create.

## Name the database

```sh
sudo editor /etc/skift/env            # DATABASE_URL=...
```

`skift.service` reads `DATABASE_URL` from `/etc/skift/env`, one `KEY=value` per line. The package creates the file on first install, mode `0640` and group `skift` since the URL carries a password, and no upgrade or removal changes it after: it is yours, and no package owns it. `/usr/share/doc/skift/env.example` is a commented template to copy from.

## Apply the schema

```sh
DATABASE_URL=postgres://skift:...@db.example/skift skift admin migrate
```

Use the URL you put in `/etc/skift/env`. `skift admin migrate` applies every migration the binary carries that the database has not seen, and prints what it applied. A database already at the current schema applies nothing, so it is safe on every deploy.

`skift serve` does the same at start. To make the service refuse a database that is behind instead, pass it `--no-migrate`.

## Create the first principal

```sh
DATABASE_URL=postgres://skift:...@db.example/skift skift admin bootstrap "$(id -u)" <organization>
```

`skift admin bootstrap <uid> <organization>` creates the organization and, in it, a `user` principal that a caller on the unix socket with that uid resolves to. It prints both. The uid is the one the `skift` CLI will run as; `$(id -u)` is the invoking user's.

Every caller that will reach the socket needs a principal; run it once per uid. It is idempotent: an organization already carrying the name and a principal already holding the uid are adopted, so provisioning can run it every time without knowing whether it has run before.

## Start the service

```sh
sudo systemctl enable --now skift.service
```

`skift.service` runs as the `skift` user. It is `Type=notify` and becomes active only once the socket is bound, the database has answered, and the schema is current. The service also runs skift's background jobs, such as releasing leases whose TTL has passed, every minute. `skift admin jobs list` shows them, and `skift admin jobs run <job>` runs one now.

## Let an account reach the socket

```sh
sudo usermod -aG skift "$USER"
```

Then start a fresh session for that account. The service identifies a caller by the uid on its end of `/run/skift/skift.sock`. The directory is group `skift` and setgid, so the socket is born in that group, and its `0660` mode admits the group.

## Check it works

```sh
skift ready
```

`skift ready` answers as the bootstrapped user. On a new deployment the ready set is empty.

## Upgrade it

Any release upgrades directly from any earlier one; no release is one you must stop at on the way. [ADR-015](adrs/015-upgrades-and-migrations.md) has the promise and what holds migrations to it, and [ADR-018](adrs/018-unattended-upgrades.md) how upgrades become unattended.

Back up the database first. It is the way back if the new release does not suit you:

```sh
pg_dump --format=custom --file=skift-before-upgrade.dump "$DATABASE_URL"
```

Then install the new package and restart the service:

```sh
sudo dnf install ./dist/skift-*.$(uname -m).rpm
sudo systemctl restart skift.service
```

Before an upgrade whose release adds migrations the database has not had, the package snapshots the database. Its pre-install step runs the installed release's `skift admin snapshot` as user `skift`, which copies every table to a file in `/var/lib/skift/backups/` and keeps the five newest. If the snapshot fails, because the database cannot be reached or the disk is full, the pre-install step fails: dnf installs nothing of the new release, the installed one keeps running, and the next upgrade tries again. The reason is in dnf's output and in the journal, under `journalctl -t skift-upgrade`. No snapshot is taken on a first install, by an upgrade that adds no migration, when `/etc/skift/env` names no database, or when the installed release is 0.4.0 or earlier, which has no `admin snapshot`. Nothing reads a snapshot back yet, so keep taking the backup above.

The service applies the new release's migrations as it starts, and reports ready only once the schema is current. Don't run `skift admin migrate` from the new release while the old one is still serving: the old binary would be answering on a schema it was not built for.

## Go back to an older release

A migration is never undone in place. To return to the release you upgraded from, restore the backup you took before upgrading:

```sh
sudo systemctl stop skift.service
pg_restore --clean --if-exists --dbname="$DATABASE_URL" skift-before-upgrade.dump
sudo dnf downgrade ./skift-<older version>.$(uname -m).rpm
sudo systemctl start skift.service
```

Anything written between the upgrade and the restore is lost. Restoring is the only way back: an older release is not built for the newer schema, and a migration's down section is a development tool, not a rollback for a live database.

## Serve callers on other machines

To serve a tailnet alongside the socket, set `SKIFT_TAILNET_HOSTNAME` in `/etc/skift/env`, with `TS_AUTHKEY` to enrol the node and `SKIFT_TAILNET_TAGS` for the ACL tags it advertises; `/usr/share/doc/skift/env.example` shows the three. Then restart `skift.service`. The node's state lives under the unit's `StateDirectory=`, `/var/lib/skift`, so a restart is the same node.

Bind each calling node to a principal with `--tailscale-node`:

```sh
DATABASE_URL=postgres://skift:...@db.example/skift skift admin bootstrap "$(id -u)" <organization> --tailscale-node <stable id>
```

The stable id is what `tailscale status --json | jq -r .Self.ID` prints on the calling machine. A caller there reaches the service with `SKIFT_ADDRESS=http://<hostname>.<tailnet>.ts.net` and is identified by its own node. [Identity by transport](architecture.md#identity-by-transport) describes how each transport resolves a caller.
