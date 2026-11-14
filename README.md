# Skift

Agent-first project management built on directed acyclic graphs.

A software project is a graph of tasks. When every dependency of a task is satisfied, the task is ready, and a ready task is something an agent can start on. Skift is the durable store for that graph: agents and humans build and mutate a backlog through one API, and orchestrators consume the set of ready work to dispatch agents in parallel.

Skift holds the plan. It does not run agents, schedule work, or define workflows. See [docs/vision.md](docs/vision.md) for why, [docs/core-principles.md](docs/core-principles.md) for the decision filters, and [docs/domain-model.md](docs/domain-model.md) for the model itself.

## Install

With [mise](https://mise.jdx.dev), which verifies the release's signature and pins who signed it:

```sh
mise use -g packslip:github.com/steading-ai/skift
```

Or download an archive from the [releases](https://github.com/steading-ai/skift/releases) and check that this repository's release workflow built it:

```sh
gh attestation verify skift_*.tar.gz --repo steading-ai/skift --signer-workflow steading-ai/skift/.github/workflows/release.yml
```

## Status

Working service, CLI, and MCP server, built from the backlog below by an autonomous agent loop. Architecture decisions are recorded in [docs/adrs/](docs/adrs/); milestones in [docs/roadmap.md](docs/roadmap.md).

Skift is built from its own backlog, in [backlog.toml](backlog.toml), expressed in skift's domain model. Each node is a self-contained work order with acceptance criteria and the scenarios its specs must cover.

```
scripts/ready.py            what can be worked now
scripts/ready.py --all      what cannot, and why
scripts/ready.py --show 18  the full work order for one node
```

An agent works one node at a time from an empty context window: read the work
order, read the documents it references, build it, leave `make lint && make
test` green.

A node whose `assignee_kind` is `user` is a gate: work only a person can
perform, such as minting a token or paying for something. Policy refuses an
agent principal that claims one, and refuses any non-user principal that
completes one; a claimed gate would leave the ready set and hide the ask from
the person waiting for it. The way to help is a node of your own that the gate
depends on — do the research there, complete it, and the person arrives at the
gate with your recommendation already in its inputs.

## Deploying

A deployment is the `skift` binary, a PostgreSQL database it may create tables
in, and the systemd units that run it. The binary carries its schema and
can seed its own first caller, so a fresh install needs no checkout, no
migration tool, and no SQL client. Install, migrate, bootstrap, start:

```sh
sudo dnf install skift
DATABASE_URL=postgres://skift:...@db.example/skift skift migrate
DATABASE_URL=postgres://skift:...@db.example/skift skift admin bootstrap "$(id -u)" homestead
sudo systemctl enable --now skift.service skift-reap.timer
```

- **The package** installs the `skift` binary, the system units, and the
  `skift` user and group the service runs as, and starts nothing: the
  service needs a database, which is not the package's to create. See
  "Install it from the rpm" below for building it.
- **`skift migrate`** applies every migration the binary carries that the
  database has not seen, and prints the versions it applied. A database
  already at the current schema applies nothing, so it is safe on every
  deploy. `skift serve` does the same at start, so this step is for running
  the migrations ahead of the service; pass `--no-migrate` to the service
  to make it refuse a database that is behind instead.
- **`skift admin bootstrap <uid> <organization>`** creates the organization
  and, in it, a `user` principal that a caller on the unix socket with that
  uid resolves to. It prints both. It is idempotent: an organization
  already carrying the name and a principal already holding the uid are
  adopted, so provisioning can run it every time without knowing whether
  it has run before. The uid is the one the `skift` CLI will run as;
  `$(id -u)` is the invoking user's. Every caller that will reach the
  socket needs a principal; run it once per uid. With `--tailscale-node
  <stable id>` the same principal is also what a caller from that tailnet
  node resolves to.
- **`skift.service`** reads `DATABASE_URL` from `/etc/skift/env`, one
  `KEY=value` per line, so put the same URL there; the package ships the
  template. It is `Type=notify` and becomes active only once the socket is
  bound, the database has answered, and the schema is current.
  `skift-reap.timer` runs `skift reap` every minute.
- **The tailnet** is served alongside the socket when the same file sets
  `SKIFT_TAILNET_HOSTNAME`, with `TS_AUTHKEY` to enrol the node and
  `SKIFT_TAILNET_TAGS` for the ACL tags it advertises; the template shows
  the three. The node's state lives under the unit's `StateDirectory=`,
  `/var/lib/skift`, so a restart is the same node. A caller on another
  machine then reaches it with `SKIFT_ADDRESS=http://<hostname>.<tailnet>.ts.net`
  and is identified by its own node, which `skift admin bootstrap
  --tailscale-node` binds to a principal.

`skift ready` then answers as the bootstrapped user, with an empty ready set
once a project exists to ask about. The caller reaches the socket through
the `skift` group; see the rpm section for how the socket admits it.

## Developing

Skift stores everything in PostgreSQL, and the behaviour worth testing (row
locking, recursive queries) only exists in a real server. Development runs
against one in a container; nothing is faked.

Prerequisites:

- [mise](https://mise.jdx.dev) — `mise install` puts every tool the Makefile
  uses on `PATH`.
- Rootless podman. This is the only system package the build assumes; the
  compose provider `podman compose` dispatches to is the `docker-compose`
  mise installs, so `podman compose version` prints one without further
  setup.
- `make build` leaves the binary at `./skift`. The harness and the plugin's
  MCP server run `skift` by name, so link it onto `PATH`, for example
  `ln -sf "$PWD/skift" ~/.local/bin/skift`.

### Bring the database up

```sh
make db-up
```

On the first run this copies `.env.example` to `.env`, then starts the
`postgres` service from `compose.yaml` and waits until it accepts TCP
connections. `.env` is gitignored; edit it if port 5432 is taken or you want
different credentials. Every variable in it is required by `compose.yaml`, so
a missing value fails at start rather than producing a database with an empty
password.

The image is pinned by digest. To move to a newer PostgreSQL, change the
digest in `compose.yaml` deliberately.

Connect with the bundled client:

```sh
podman compose exec postgres psql -U skift
```

or from the host with anything that reads `DATABASE_URL` from `.env`.

### Apply the schema

```sh
make db-migrate
```

Runs every pending goose migration from `internal/store/migrations` against
the development database. The same files are embedded in the binary, so this
is only for working on the schema by hand; specs migrate their own databases.

### Reset the database

```sh
make db-reset
```

Drops and recreates the development database inside the running container.
The container and its volume survive, so this takes about a second. To
destroy everything including the volume:

```sh
podman compose down -v
```

### Stop the database

```sh
make db-down
```

Data stays in the `skift_pgdata` volume for the next `make db-up`.

### Run the suite

```sh
make test        # everything
make test-unit   # specs that need no external services
make test-db     # specs labelled "db"
make lint
```

`hk install` wires `make lint` and `make test-unit` into a pre-commit hook
(see `hk.pkl`); `hk check` runs the same steps by hand.

Specs labelled `db` need podman. Store specs start their own PostgreSQL
through testcontainers rather than using the compose stack, so they never
touch your development data. The compose specs at the repository root
exercise `make db-up` and `make db-reset` under a separate compose project
name (`skift-spec`) on a random port, and remove it when they finish.

Suites run their specs in parallel processes. A suite that needs the
database starts one container in its first process through
`internal/testsupport`, migrates a template database once, and hands every
`db`-labelled spec a fresh clone of it that is dropped when the spec ends:

```go
var pg *testsupport.Postgres

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	started, err := testsupport.Start(ctx, migrate)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(started.Stop)
	return []byte(started.URL())
}, func(ctx context.Context, adminURL []byte) {
	var err error
	pg, err = testsupport.Attach(ctx, string(adminURL))
	Expect(err).NotTo(HaveOccurred())
})

It("reads its own database", Label("db"), func(ctx SpecContext) {
	db := pg.Database(ctx) // a *pgxpool.Pool on a database nobody else sees
})
```

The harness finds the rootless podman socket on its own. Set `DOCKER_HOST`
to point it somewhere else.

### Install it from the rpm

`make package` cross-compiles `skift` and builds an rpm for `x86_64` and
`aarch64` into `dist/`. The package carries the binary, the system units in
`contrib/systemd/`, and the fragments under `packaging/` that create the
`skift` system user and group and the socket directory:

```sh
make package
sudo dnf install ./dist/skift-*.$(uname -m).rpm
sudo editor /etc/skift/env            # DATABASE_URL=...
sudo systemctl enable --now skift.service skift-reap.timer
```

The package installs but does not start anything: the service needs a
database, which is not the package's to create. `skift.service` runs as
the `skift` user and is `Type=notify`, active only once the socket is bound
and the database has answered. `skift-reap.timer` runs `skift reap` every
minute, releasing leases whose TTL has passed; the command is idempotent
and safe to run by hand alongside the timer.

The service identifies a caller by the uid on its end of
`/run/skift/skift.sock`. The directory is group `skift` and setgid, so the
socket is born in that group, and its `0660` mode admits the group; add an
account to the `skift` group and give it a fresh session to let it call.

`make version` prints what `skift version` will report: the tag on a clean
tagged checkout, and otherwise the nearest tag carrying a development
marker such as `+dev.3.g1a2b3c4.dirty`.

## Documentation

- [Vision](docs/vision.md)
- [Core principles](docs/core-principles.md)
- [Domain model](docs/domain-model.md)
- [Architecture](docs/architecture.md)
- [Roadmap](docs/roadmap.md)
- [Retrospectives](docs/retrospectives/)
- [Current plan: prove the bet](docs/plans/2026-09-prove-the-bet.md)
- [Architecture decision records](docs/adrs/)

## License

Skift is released under the [ISC license](LICENSE.md).
