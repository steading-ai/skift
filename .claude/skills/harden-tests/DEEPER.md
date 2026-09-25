# Beyond examples: models, fuzzing, races

## A state machine, checked against a model

Where the domain holds a lifecycle (a node that is open, done or dropped,
held or not, leased or not), write the rules down as a model and let rapid
drive random action sequences against the real service. No library beyond
`pgregory.net/rapid`, which has this built in:

```go
checkProperty(func(t *rapid.T) {
	m := newModel(ctx, t, s)            // fresh org, one seed node, model state
	t.Repeat(map[string]func(*rapid.T){
		"":       m.check,              // invariant: stored row == model, after every action
		"claim":  m.claim,              // model predicts accept/refuse and the sentinel
		"finish": func(t *rapid.T) { if !m.live { t.Skip("no lease") }; m.finish(t) },
		// complete, drop, reopen, hold, unhold, heartbeat, release, expire ...
	})
})
```
- Each action performs the real call, asserts the model's prediction
  (success, or `errors.Is(err, TheSentinel)`), then advances the model.
- `t.Skip` marks an action inapplicable in the current state.
- Prove the model bites before trusting it: break one rule on purpose, see
  it fail, restore. rapid writes a `.fail` file under `testdata/rapid/`
  when it fails; delete the one your deliberate break produced, not the
  committed ones.
- The model's rules come from reading the code's preconditions, not from
  the docs alone. Where the two disagree, that is a finding for the
  maintainer, and the spec pins the code as it is.

## Fuzz targets, carved out of the framework rule

A lint rule that keeps `testing` out of Ginkgo suites blocks `testing.F`.
Carve out a file pattern rather than loosening the rule:
```yaml
# .golangci.yml
forbidigo:
  forbid:
    - pattern: ^testing\.
      msg: use Ginkgo and Gomega; testing belongs only in *_suite_test.go and *_fuzz_test.go
exclusions:
  rules:
    - path: (_suite|_fuzz)_test\.go$
      linters: [forbidigo]
```
Target every parser of bytes a client chooses (wire codec, cursor decoder,
reference parser): seed with valid and malformed inputs, assert no panic,
and that anything accepted survives a round trip unchanged. `go test`
drives one target at a time:
```make
FUZZ_TIME ?= 20s
fuzz:
	go test ./internal/codec -run '^$$' -fuzz '^FuzzUnmarshal$$' -fuzztime $(FUZZ_TIME)
	go test ./internal/domain -run '^$$' -fuzz '^FuzzDecodeCursor$$' -fuzztime $(FUZZ_TIME)
```

## The race detector

```make
test-race: test-policy
	$(GINKGO) -race ./...
```
Cheap, and nothing else finds a data race the specs happen not to trip.
Put it in CI as a second job beside the main one, with `make fuzz`, so
neither lengthens the gate.

## What the mutation survivors still leave

Retry loops, conflict detectors and back-off are reachable only by a
race. A stress spec that fires N concurrent claims at one ready node and
asserts exactly one lease exercises them deterministically enough; that
and connection-kill fault injection on the listener are the next step.
