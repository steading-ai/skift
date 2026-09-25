# Mutation testing with gremlins

gremlins changes one covered statement at a time (negates a condition,
moves a boundary, flips an increment) and reruns the package's tests. A
mutant the tests let live is a check no spec makes. The score is only as
good as the run's validity, so the setup comes first.

## Install and configure

```toml
# mise.toml
[tools]
"go:github.com/go-gremlins/gremlins/cmd/gremlins" = "0.6.0"
```

Settings nest under the command. A flat `workers: 6` is ignored without a
word and the run takes the defaults:
```yaml
# .gremlins.yaml
unleash:
  workers: 12              # ~1.5× cores when suites wait on a database
  timeout-coefficient: 6   # × the solo baseline; contention needs margin
  threshold:
    efficacy: 80
```
After starting a run, confirm what it is actually doing:
```
pgrep -af 'go test -timeou[t]'   # one line per worker; the -timeout is coefficient × baseline
```

## The Make target

Adapt the names: the shared-server variable and the property-checks knob
are whatever the target repo's test harness reads, and the package is its
core.
```make
PROPERTY_CHECKS ?= 100
MUTATE_FLAGS := $(if $(DIFF),--diff $(DIFF),)
mutate: db-up
	set -a && . ./.env && set +a && \
	  <REPO>_TEST_POSTGRES_URL="$$DATABASE_URL" <REPO>_PROPERTY_CHECKS=$(PROPERTY_CHECKS) GOFLAGS=-count=1 \
	  gremlins unleash $(MUTATE_FLAGS) ./internal/<core>
```
- `GOFLAGS=-count=1`: gremlins times each mutant against the unmutated
  run, and `go test` answers a cached run in milliseconds. Without this
  every mutant times out inside a minute.
- **One shared database.** Twelve workers each starting a testcontainer
  race on podman's networks; a failed teardown fails a suite whose specs
  passed, and gremlins counts that as a kill. Give the harness a mode that
  attaches to a running server (an env var carrying its URL), with a
  per-suite template database named in the URL fragment, dropped at exit.
- **Durability off** on that server (`postgres -c fsync=off -c
  synchronous_commit=off -c full_page_writes=off`): every `DROP DATABASE`
  forces a checkpoint, and a suite that clones and drops a database per
  spec makes the single checkpointer the bottleneck while the CPU idles.
- **Property checks knob**: thousand-history property specs are most of a
  serial run; a hundred keeps them in play at a tenth of the cost.
- `DIFF=<ref>` mutates changed lines only: the everyday form, and the one
  for a reviewer's checklist.

Run it in the background with a Monitor printing counts every five
minutes, and run **nothing else**: not a focused spec, not `make cover`.
Anything else on the box turns kills into timeouts. Write specs during the
run if you like; verify them after. Killed suites leave orphan databases;
drop them before the next run.

## Reading the result

`Test efficacy` is killed over killed plus lived; timeouts are neither.
Timeouts that land on the same lines across two runs are mutants that hang
a retry loop, not contention. Then, per `LIVED` line, read the site and
ask what observable differs:

| pattern | verdict | action |
|---|---|---|
| a boundary a later step neutralises (trim, min, equal strings) | equivalent | none |
| a retry counter, conflict detector, or back-off only a race reaches | race-only | none; a stress spec later |
| a timestamp byte, a carry after 2^72 increments | equivalent for our lifetime | none |
| a field the code documents as part of a contract (policy input, event before-image, cursor, page boundary) | real gap | spec |
| a count or filter that changes what a caller receives | real gap | spec |

For every real gap, **apply the mutant by hand and watch the spec fail**:
```sh
cp $F $S/orig; sed -i '390s/Size > 0/Size >= 0/' $F
go tool ginkgo --focus-file the_spec_test.go ./internal/domain && echo SURVIVED || echo killed
cp $S/orig $F
```
A spec written from the survivor's line but never run against it is a
guess. Keep the verdict table; it is what the next person reads.

Before reporting a score, one final run against the committed tree with no
edits in flight: gremlins lists mutants from the live tree and copies it per
worker, so a run with edits mid-flight is not a clean figure.
