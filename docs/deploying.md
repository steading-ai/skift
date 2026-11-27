# How to deploy skift from the rpm

This guide shows how to run the skift service under systemd on Fedora, installed from [pkgs.stead.ing](https://pkgs.stead.ing), Steading's package repository: add the repository, install skift and point it at a database, let it upgrade itself, and go back when a release does not suit you. To run the service in containers instead, and for the CLI and agent setup that follow either way, see [How to install skift](install.md).

You need:

- A Fedora host with systemd and dnf5. The repository serves every Fedora version.
- A PostgreSQL database skift may create tables in. The binary carries its schema and can seed its own first caller, so the host needs no migration tool and no SQL client.

## Add the repository

Download the signing key and print its fingerprint:

```sh
curl -fsSLO https://pkgs.stead.ing/RPM-GPG-KEY-steading
gpg --show-keys RPM-GPG-KEY-steading
```

It must be `9C08AF55275334E2D86259174F5C40EB80780443`. If it is any other, stop. Otherwise import the key, then install the `steading-release` package, which configures the repository and carries the same key:

```sh
sudo rpm --import RPM-GPG-KEY-steading
sudo dnf install --setopt=localpkg_gpgcheck=1 https://pkgs.stead.ing/steading-release.noarch.rpm
```

`--setopt=localpkg_gpgcheck=1` makes dnf check the package's signature against the key you imported; without it, dnf installs a package named by its URL unchecked.

Instead of the package, you can add the repository's `.repo` file, which names the key by its URL:

```sh
sudo dnf config-manager addrepo --from-repofile=https://pkgs.stead.ing/steading.repo
```

Either way, `gpgcheck` and `repo_gpgcheck` are on: dnf reads no repository metadata and installs no package the key did not sign. The first time dnf reads a channel, it prints the key's fingerprint and asks whether to import it. Answer `y` only if the fingerprint is the one above. `config-manager` is in the `dnf5-plugins` package; if dnf has no such command, install that first.

### Choose a channel

The repository has two channels. `steading`, which is `stable`, is enabled: it gets a release once that release has been in `edge` for two days and has not been withdrawn. `steading-edge` gets every release as soon as it is published. To follow `edge` instead:

```sh
sudo dnf config-manager setopt steading.enabled=0 steading-edge.enabled=1
```

To return to `stable`, swap the two values. dnf does not downgrade on its own, so the host stays on the release it has until `stable` passes it. [ADR-017](adrs/017-release-channels-and-packages.md) describes the channels and how a release moves from one to the other.

If you change channel after skift is installed, refresh the metadata and accept the key, as dnf asks:

```sh
sudo dnf makecache
```

dnf5-automatic cannot answer that question. It skips a channel whose key it has not been given, and reports no error.

## Install skift

```sh
sudo dnf install skift
```

The package installs the `skift` binary, the system units, and the `skift` user and group the service runs as, and starts nothing: the service needs a database, which is not the package's to create.

A host that already runs skift from a release's rpm needs no new install. With the repository added, `sudo dnf upgrade skift` moves it to the channel's newest release. If the installed release is 0.4.0 or earlier, that upgrade takes no snapshot, so back up the database first.

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

## Keep it current

Install dnf5-automatic:

```sh
sudo dnf install dnf5-plugin-automatic
```

Have it install the updates it finds, not only download them, by setting `apply_updates` in `/etc/dnf/automatic.conf`:

```sh
sudo editor /etc/dnf/automatic.conf
```

```ini
[commands]
apply_updates = yes
```

Then enable its timer:

```sh
sudo systemctl enable --now dnf5-automatic.timer
```

The timer runs once a day, and each run upgrades every package on the host, not only skift. [dnf5-automatic's documentation](https://dnf5.readthedocs.io/en/latest/dnf5_plugins/automatic.8.html) has its other settings: which updates to apply, where to send the report, and when to reboot. To upgrade skift now instead of waiting, run `sudo dnf upgrade skift`.

### What an upgrade does

When dnf installs a new release of skift, four things happen in turn:

1. **Snapshot.** If the new release carries migrations the database has not had, the package's pre-install step has the installed release copy every table to a file in `/var/lib/skift/backups/`, keeping the five newest. If it cannot, because the database is unreachable or the disk is full, nothing of the new release is installed, the installed release keeps running, and the next run tries again.
2. **Install.** dnf replaces the binary and the units. `/etc/skift/env` is left as it is.
3. **Restart.** A running service is restarted onto the new binary. A stopped one stays stopped.
4. **Migrate.** The service applies the new release's migrations as it starts, and becomes active only once the schema is current.

[ADR-018](adrs/018-unattended-upgrades.md) has why upgrades work this way, and [ADR-015](adrs/015-upgrades-and-migrations.md) the promise that any release upgrades directly from any earlier one.

### When an upgrade fails

Look in these places, in this order:

- **dnf's report.** `journalctl -u dnf5-automatic.service` shows what each run found and installed. It leaves out what the package's own steps printed; `/var/log/dnf5.log` records each of those steps and its return code.
- **The snapshot.** `journalctl -t skift-upgrade` has a line for every upgrade: the snapshot it took, why it took none, or why the snapshot failed and which release is still running.
- **The service.** `journalctl -u skift` says why a release did not start, such as a schema it cannot serve.
- **A workstation.** `skift doctor` says whether the service answers, and how to fix what is not right.

If skift never upgrades, and dnf5-automatic's report ends at `Is this ok [y/N]:` after the key's fingerprint, dnf has not been given the key for that channel. Run `sudo dnf makecache` and accept it.

## Go back a release

When a new release misbehaves, downgrade to the one before it, and hold it there:

```sh
sudo dnf downgrade skift
sudo dnf versionlock add skift
```

`dnf downgrade skift` installs the release before the installed one, and restarts the service onto it, keeping every write. To go back further, name the release, as `skift-<version>`; `sudo dnf list --showduplicates skift` lists the releases the channel has. `dnf versionlock add skift` holds the package at that release, or dnf5-automatic upgrades it again on its next run. Once a release that fixes the problem is out, let upgrades resume:

```sh
sudo dnf versionlock delete skift
```

An older release serves a newer schema only when the schema's compatibility floor allows it. When it does not, the service does not start, and `journalctl -u skift` says so:

```text
skift serve: apply migrations: schema <current> needs migration <floor>, and this build's newest is <newest>: install skift that carries migration <floor>, or restore a snapshot
```

Either upgrade again to a release that carries that migration, with `sudo dnf versionlock delete skift` and `sudo dnf upgrade skift`, or [restore the snapshot](#restore-a-snapshot) taken before the upgrade. Releases 0.4.0 and earlier have no floor and refuse every schema newer than their own, so going back to one of them always means restoring. On a host that upgrades at least weekly, the release it ran before is always within the floor; [ADR-018](adrs/018-unattended-upgrades.md#decision-outcome) has the rule.

## Restore a snapshot

A restore returns the database to exactly what it held when the snapshot was taken, before the upgrade migrated it. Every write made since is lost: whatever people and agents recorded after the upgrade began. When a downgrade works, prefer it.

The snapshots are in `/var/lib/skift/backups/`, each named for when it was taken and the schema it was taken at, as `skift-snapshot-<time>-schema<version>.zip`. The upgrade's line in `journalctl -t skift-upgrade` names the one it took.

```sh
sudo ls /var/lib/skift/backups/
```

Stop the service, so that nothing writes to the database during the restore:

```sh
sudo systemctl stop skift.service
```

Restore the snapshot over what the database holds now:

```sh
sudo DATABASE_URL=postgres://skift:...@db.example/skift skift admin restore --clean /var/lib/skift/backups/<snapshot>
```

`--clean` drops everything in the database's schema first; without it, restore refuses a database that holds anything. Restore checks the file before it changes anything, and prints what it restored: `taken_by` is the release that took the snapshot, and `schema_version` the schema the database is now at. `skift admin restore --help` lists what it refuses.

Go back to the release that took the snapshot, hold it there, and start the service:

```sh
sudo dnf downgrade skift-<taken_by>
sudo dnf versionlock add skift
sudo systemctl start skift.service
```

Start the newer release instead, and it migrates the restored database again as it starts.

## Serve callers on other machines

To serve a tailnet alongside the socket, set `SKIFT_TAILNET_HOSTNAME` in `/etc/skift/env`, with `TS_AUTHKEY` to enrol the node and `SKIFT_TAILNET_TAGS` for the ACL tags it advertises; `/usr/share/doc/skift/env.example` shows the three. Then restart `skift.service`. The node's state lives under the unit's `StateDirectory=`, `/var/lib/skift`, so a restart is the same node.

Bind each calling node to a principal with `--tailscale-node`:

```sh
DATABASE_URL=postgres://skift:...@db.example/skift skift admin bootstrap "$(id -u)" <organization> --tailscale-node <stable id>
```

The stable id is what `tailscale status --json | jq -r .Self.ID` prints on the calling machine. A caller there reaches the service with `SKIFT_ADDRESS=http://<hostname>.<tailnet>.ts.net` and is identified by its own node. [Identity by transport](architecture.md#identity-by-transport) describes how each transport resolves a caller.
