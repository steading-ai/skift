# `/harden-tests` — Tests That Catch Bugs, Not Coverage That Flatters

Takes a Go repository's test suite from a coverage number to a suite that
would notice a real defect: measures hand-written coverage honestly and
gates it, mutation-tests the core and turns every survivor into a spec or a
written verdict, adds a model-checked state machine, fuzz targets and the
race detector, fixes what the new specs expose, and leaves the maintainer a
short list of what only they can decide.

```
/harden-tests grade
/harden-tests ./pkg/engine
/harden-tests --diff main
```

## Why this exists

**The coverage number lies in both directions.** Generated code drags it
down and nobody knows what to do about it; a spec that calls a function and
asserts nothing pushes it up. In the repository this skill was written
against, 80% coverage hid an audit event written without the reason its
schema required, two malformed-request paths that surfaced as internal
errors, and an authorization refusal that crashed its own audit. Those were found by specs written for
behaviour and by mutants nobody's specs noticed, not by chasing the figure.

**Mutation testing is easy to run wrong and hard to tell.** The first run
here reported every mutant timed out because the test cache answered the
baseline in 200 ms; the second used sixteen workers and a tight timeout
because the config file's keys were at the wrong level and silently ignored;
the third was starved by a focused spec run on the same box. Each looked
like a bad suite. MUTATION.md holds what a valid run needs, in the order it
was learned.

**Survivors are the deliverable, and most of them are not bugs.** A boundary
that a later `Trim` neutralises, a retry counter only a race reaches, a
timestamp byte that matters in the year 10889: each needs a verdict written
down, or the next person re-derives it. The ones that are real need a spec
proved against the mutant by hand, or the spec is a guess.

## How it works

Eight steps, in SKILL.md: orient, measure, gate, close the gaps, mutate,
go deeper, record, report. Each step ends in a commit. The heavy runs
(mutation, race) go to the background under a Monitor and nothing else
runs beside them, because contention turns kills into timeouts.

## What it does NOT produce

A push, a pull request, or a change to CI credentials. The CI workflow it
writes is unverified until the maintainer pushes it. It does not raise
coverage by covering dead code: dead code is deleted.

## Files

- **COVERAGE.md** — the merged, excluded, gated coverage recipe and how to read the gaps
- **MUTATION.md** — gremlins setup, why each setting exists, the survivor verdict table, hand-verification
- **DEEPER.md** — the rapid state-machine model, the fuzz carve-out, the race target
