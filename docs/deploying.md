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

`skift.service` reads `DATABASE_URL` from `/etc/skift/env`, one `KEY=value` per line; the package ships the template.

## Apply the schema

```sh
DATABASE_URL=postgres://skift:...@db.example/skift skift migrate
```

Use the URL you put in `/etc/skift/env`. `skift migrate` applies every migration the binary carries that the database has not seen, and prints what it applied. A database already at the current schema applies nothing, so it is safe on every deploy.

`skift serve` does the same at start. To make the service refuse a database that is behind instead, pass it `--no-migrate`.

## Create the first principal

```sh
DATABASE_URL=postgres://skift:...@db.example/skift skift admin bootstrap "$(id -u)" <organization>
```

`skift admin bootstrap <uid> <organization>` creates the organization and, in it, a `user` principal that a caller on the unix socket with that uid resolves to. It prints both. The uid is the one the `skift` CLI will run as; `$(id -u)` is the invoking user's.

Every caller that will reach the socket needs a principal; run it once per uid. It is idempotent: an organization already carrying the name and a principal already holding the uid are adopted, so provisioning can run it every time without knowing whether it has run before.

## Start the service

```sh
sudo systemctl enable --now skift.service skift-reap.timer
```

`skift.service` runs as the `skift` user. It is `Type=notify` and becomes active only once the socket is bound, the database has answered, and the schema is current. `skift-reap.timer` runs `skift reap` every minute, releasing leases whose TTL has passed; the command is idempotent and safe to run by hand alongside the timer.

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

## Serve callers on other machines

To serve a tailnet alongside the socket, set `SKIFT_TAILNET_HOSTNAME` in `/etc/skift/env`, with `TS_AUTHKEY` to enrol the node and `SKIFT_TAILNET_TAGS` for the ACL tags it advertises; the template shows the three. Then restart `skift.service`. The node's state lives under the unit's `StateDirectory=`, `/var/lib/skift`, so a restart is the same node.

Bind each calling node to a principal with `--tailscale-node`:

```sh
DATABASE_URL=postgres://skift:...@db.example/skift skift admin bootstrap "$(id -u)" <organization> --tailscale-node <stable id>
```

The stable id is what `tailscale status --json | jq -r .Self.ID` prints on the calling machine. A caller there reaches the service with `SKIFT_ADDRESS=http://<hostname>.<tailnet>.ts.net` and is identified by its own node. [Identity by transport](architecture.md#identity-by-transport) describes how each transport resolves a caller.
