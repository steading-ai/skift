# How to develop skift

This guide shows how to work on skift from a checkout: build the binary, run a development database, and run the specs and the linters. [The guide for coding agents](../AGENTS.md) lists the `make` commands a change is checked with and the rules it is held to.

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

## Build the binary

```sh
mise install
make build
```

`make build` leaves the binary at `./skift`. The plugin's MCP server runs `skift` by name, so link it onto `PATH`, for example `ln -sf "$PWD/skift" ~/.local/bin/skift`.

`make version` prints what `skift version` will report: the tag on a clean
tagged checkout, and otherwise the nearest tag carrying a development
marker such as `+dev.3.g1a2b3c4.dirty`.

## Bring the database up

```sh
make db-up
```

On the first run this copies `.env.example` to `.env`, then starts the
`postgres` service from `compose.yaml` and waits until it accepts TCP
connections. `.env` is gitignored; edit it if port 5432 is taken or you want
different credentials. Every variable in it is required by `compose.yaml`, so
a missing value fails at start rather than producing a database with an empty
password.

The image is pinned by digest, in `compose.yaml` and in `internal/testsupport/postgres.go`, so the specs and the development database run one PostgreSQL. Renovate moves both pins in one pull request; see [Dependency updates](ci.md#dependency-updates).

Connect with the bundled client:

```sh
podman compose exec postgres psql -U skift
```

or from the host with anything that reads `DATABASE_URL` from `.env`.

## Apply the schema

```sh
make db-migrate
```

Runs every pending goose migration from `internal/store/migrations` against
the development database. The same files are embedded in the binary, so this
is only for working on the schema by hand; specs migrate their own databases.

## Reset the database

```sh
make db-reset
```

Drops and recreates the development database inside the running container.
The container and its volume survive, so this takes about a second. To
destroy everything including the volume:

```sh
podman compose down -v
```

## Stop the database

```sh
make db-down
```

Data stays in the `skift_pgdata` volume for the next `make db-up`.

## Run the suite

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

The harness finds the rootless podman socket on its own. Set `DOCKER_HOST`
to point it somewhere else.

## Write a spec that needs the database

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
