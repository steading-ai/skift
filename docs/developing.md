# How to develop skift

This guide shows how to work on skift from a checkout: build the binary, run a development database, and run the specs and the linters. [The guide for coding agents](../AGENTS.md) lists the `make` commands a change is checked with and the rules it is held to.

Skift stores everything in PostgreSQL, and the behaviour worth testing (row locking, recursive queries) only exists in a real server. Development runs against one in a container; nothing is faked.

Prerequisites:

- [mise](https://mise.jdx.dev) — `mise install` puts every tool the Makefile uses on `PATH`.
- Rootless podman. This is the only system package the build assumes; the compose provider `podman compose` dispatches to is the `docker-compose` mise installs, so `podman compose version` prints one without further setup.

## Build the binary

```sh
mise install
make build
```

`make build` leaves the binary at `./skift`. The plugin's MCP server runs `skift` by name, so link it onto `PATH`, for example `ln -sf "$PWD/skift" ~/.local/bin/skift`.

`make version` prints what `skift version` will report: the tag on a clean tagged checkout, and otherwise the nearest tag carrying a development marker such as `+dev.3.g1a2b3c4.dirty`.

## Bring the database up

```sh
make db-up
```

On the first run this copies `.env.example` to `.env`, then starts the `postgres` service from `compose.yaml` and waits until it accepts TCP connections. `.env` is gitignored; edit it if port 5432 is taken or you want different credentials. Every variable in it is required by `compose.yaml`, so a missing value fails at start rather than producing a database with an empty password.

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

Runs every pending goose migration from `internal/store/migrations` against the development database. The same files are embedded in the binary, so this is only for working on the schema by hand; specs migrate their own databases.

## Reset the database

```sh
make db-reset
```

Drops and recreates the development database inside the running container. The container and its volume survive, so this takes about a second. To destroy everything including the volume:

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

`make lint` is `hk check --all`: hk runs each check in `hk.pkl` as its own step, in parallel. `hk check` alone checks only the files you have changed, and `hk fix` applies the fixes the tools offer, such as formatting. `hk install` wires the same steps, then `make test-unit`, into a pre-commit hook over the files a commit stages; golangci-lint and go vet still check the whole module whenever a Go file is staged, because the package-boundary rules need every package.

Specs labelled `db` need podman. Store specs start their own PostgreSQL through testcontainers rather than using the compose stack, so they never touch your development data. The compose specs at the repository root exercise `make db-up` and `make db-reset` under a compose project of their own, named `skift-spec-` and a random suffix, on a random port, and remove it when they finish, even when a spec fails. Every name a spec gives podman, a compose project, a container or an image, is new on each run, so suites in parallel worktrees on one machine never share one.

The harness finds the rootless podman socket on its own. Set `DOCKER_HOST` to point it somewhere else.

## Run the downgrade spec

```sh
go tool ginkgo --label-filter=downgrade ./internal/store/migrations
```

The downgrade spec, in `internal/store/migrations/downgrade_test.go`, holds [ADR-018](adrs/018-unattended-upgrades.md)'s promise that `dnf downgrade` brings back a serving release. For every release tag whose newest migration meets the current schema's compatibility floor, it fetches that release's archive for this platform, checks it against the release's `SHA256SUMS`, and runs the release's `skift serve` against the schema this tree builds, with a row in every table. The release has to report ready, create two linked nodes, claim one and complete it through its socket, and stop cleanly. The spec is labelled `downgrade` and `slow`, and `make test` and CI run it with the rest.

Archives are kept under `skift/releases` in the user cache directory, `~/.cache/skift/releases` on Linux, one directory per tag, so a run that finds every eligible archive there needs no network. Each release below the floor is skipped, and the output names its newest migration and the floor. Until a release carries the floor check, which arrived in migration 29, no release is eligible, and the spec skips itself as vacuous.

Beside it, a binary built from this tree stands in for a release twice: it serves a schema that a newer migration only added to, and a migration that drops a column without raising the floor fails it, with the failure naming the binary. To hold another build to the spec as though it were an eligible release, a build of an earlier commit for instance, name it in `SKIFT_DOWNGRADE_STANDINS`, with several separated as in `PATH`:

```sh
SKIFT_DOWNGRADE_STANDINS=/path/to/older/skift go tool ginkgo --label-filter=downgrade ./internal/store/migrations
```

## Run the mutation tests

```sh
make mutate DIFF=main         # gremlins over the lines in internal/domain that differ from main
make mutate PKG=internal/cli  # another package, every line
make mutate-store DIFF=main   # the store's changed lines, judged by every suite in the module
make mutate-sql               # each rule of the store's SQL, broken in turn
```

Mutation testing changes one operator at a time and reruns the specs; a mutant the specs do not notice has lived, and each survivor is either an equivalent change or a missing spec. Read the survivors rather than the score. Runs use the development database from `make db-up` and take minutes to hours, so run nothing else against it meanwhile: contention times mutants out, and a timed-out mutant drops out of the score.

`make mutate` judges a package by its own specs. The store's Go code is exercised by the domain and server specs, so `make mutate-store` runs every suite for each mutant, with `SKIFT_MUTATING=1` set. That variable makes two suites skip themselves: the specs at the repository root, which run `make lint` in copies of the tree, and the acceptance scenarios, which build and serve the binary. Neither reaches the store, and either would cost minutes per mutant.

gremlins mutates Go operators, and the store's rules are SQL: what makes a node ready, which holds an empty ready page reports, which cycles a patch is refused for. `make mutate-sql` breaks each of those rules in turn and runs the store, domain and server specs against it. Each row of the table in `internal/tools/mutatesql/mutants.go` names a file, an exact piece of SQL that occurs in it once, what replaces it, and the rule that breaks. The run prints the spec that killed each mutant, restores every file it changed even when interrupted, and fails if any mutant lives. For a query sqlc generates, the row edits the string in the `.sql.gen.go` file, because that string is what runs.

When you add a rule, add a row that breaks it. When you rewrite a rule's SQL, update its row: the spec "The table applies to the tree" fails in `make test` while any row's text no longer occurs exactly once.

## Write a spec that needs the database

Suites run their specs in parallel processes. A suite that needs the database starts one container in its first process through `internal/testsupport`, migrates a template database once, and hands every `db`-labelled spec a fresh clone of it that is dropped when the spec ends:

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
