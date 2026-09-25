---
name: harden-tests
description: Use when asked to raise a Go repository's test suite from a coverage number to tests that catch real bugs. Measures hand-written coverage across every test binary and gates it, runs mutation testing and turns each survivor into a spec or a written verdict, adds property, state-machine, fuzz and race checks, fixes the defects the specs expose at the root, and records what the maintainer must decide.
argument-hint: "[grade | ./pkg/... | --diff <ref>]"
disable-model-invocation: true
allowed-tools:
  - Read
  - Grep
  - Glob
  - Edit
  - Write
  - Bash(make cover:*)
  - Bash(make mutate:*)
  - Bash(make test:*)
  - Bash(make test-unit:*)
  - Bash(make test-db:*)
  - Bash(make test-race:*)
  - Bash(make fuzz:*)
  - Bash(make lint:*)
  - Bash(make db-up:*)
  - Bash(make generate:*)
  - Bash(go test:*)
  - Bash(go tool:*)
  - Bash(go vet:*)
  - Bash(gofmt:*)
  - Bash(mise install:*)
  - Bash(mise use:*)
  - Bash(git status:*)
  - Bash(git diff:*)
  - Bash(git log:*)
  - Skill(commit)
---

# Harden Tests

**Autonomy:** human-only · acts autonomously in the working tree: writes specs and root-cause fixes, commits each step through `/commit`, never pushes. Long jobs (mutation, race) run in the background under a Monitor, and nothing else runs beside them.

## Arguments
```
$ARGUMENTS
```
Empty or `grade`: measure, give a letter grade and what an A+ would take, change nothing. A package path is the package to mutation-test (default: the domain or core package). `--diff <ref>` mutates the lines that diff touches only.

## Instructions

1. **Orient.** Read the repo's agent instructions and Makefile: the test framework, how a database is provided (testcontainers, compose), what is generated, which lint rules carry design decisions. Tools come through `mise`; never brew, apt or npx.
2. **Measure** hand-written coverage: [COVERAGE.md](COVERAGE.md). Exclude generated code and unreachable transports, each with a written reason; merge every test binary's profile, including one built with `-cover` for end-to-end scenarios. Grade: below 70 is a C, 70 to 85 a B, 85 with a gate a B+. An A+ is the rest of this list.
3. **Gate it.** A baseline file set to the measurement minus its run-to-run jitter, a `make cover` that fails beneath it, and the same target in CI.
4. **Close the gaps that matter.** List uncovered statements per file, then decide per gap: dead code → delete it, own commit · reachable only through a tailnet or a killed connection → exclude it with the reason · anything else → a spec named for the behaviour. A spec that fails against the code has found a defect: fix it at the root in its own `fix(...)` commit; never bend the spec to the bug.
5. **Mutation-test** the core package: [MUTATION.md](MUTATION.md). First make the score valid (test cache off, one shared database, workers sized to the box, nothing else running), then read the survivors rather than the score. Every survivor gets a verdict: equivalent · reachable only by a race or as timing · real gap. A real gap gets a spec **verified by applying the mutant by hand** and watching the spec fail. Keep the verdict list; it is the deliverable.
6. **Go deeper** where the domain holds state: [DEEPER.md](DEEPER.md). A rapid `t.Repeat` model of the state machine, a fuzz target for every parser of client-chosen bytes (carved out of the framework lint rule), a race-detector target, all three in CI beside the main job.
7. **Record for the maintainer**, where they say (a note, an issue): defects fixed, discrepancies between code and documents, decisions only they can make. Not a changelog; `git log` is the changelog.
8. **Report** against the criteria set at the start, with the figures and the survivor verdicts. Do not push.

Rules that each cost an hour to learn:
- Never run any suite while mutation testing runs: the mutants time out and the score is hollow.
- Trust a tool's effective settings (`pgrep -af`), not its config file; a key in the wrong place is silently ignored.
- Keep what stays unstaged compiling when committing: a pre-commit hook may stash it.

## Examples
```
/harden-tests grade               → the figure, a letter grade, what A+ takes; no changes
/harden-tests ./internal/domain   → the whole playbook, mutation testing that package
/harden-tests --diff main         → survivors on this branch's changed lines only
```
