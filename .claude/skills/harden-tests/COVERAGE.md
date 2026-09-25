# Hand-written coverage, merged across every test binary

The raw `go test -cover` number counts generated code, which is large and
half-covered, and misses whatever an end-to-end suite exercises in a binary
it built. Both distort the figure in the direction of "nothing to do here".

## What to measure

1. **Every package's specs**, with `-coverpkg` set to the whole module so a
   spec in one package credits the code it reaches in another:
   ```
   go tool ginkgo -r -p --cover --coverpkg=github.com/org/repo/... --coverprofile=specs.out --output-dir=.cover ./...
   # plain go test:
   go test ./... -coverpkg=./... -coverprofile=.cover/specs.out
   ```
   Skip the end-to-end package here; it runs the binary, not the code.
2. **The binary under the end-to-end scenarios**, built with coverage and
   told where to write its counters. Whatever builds it (gexec.Build, `go
   build`) honours `GOFLAGS`:
   ```
   GOFLAGS='-cover -coverpkg=github.com/org/repo/...' GOCOVERDIR=$PWD/.cover/bin go tool ginkgo ./tests/acceptance
   go tool covdata textfmt -i=.cover/bin -o=.cover/acceptance.out
   ```
   The scenarios must pass `GOCOVERDIR` through to the process they start.
3. **Merge block by block**, taking a block as covered if either profile
   covered it:
   ```
   awk 'FNR == 1 { next } { k = $1 " " $2; c = ($3 > 0); if (!(k in m) || c > m[k]) m[k] = c }
        END { print "mode: set"; for (k in m) print k, m[k] }' .cover/specs.out .cover/acceptance.out > .cover/merged.out
   ```
4. **Exclude** by regex over profile lines, from a committed file where every
   line carries its reason. Match the profile's `path:line.col` form, so
   `\.gen\.go:` rather than `\.gen\.go$`:
   ```
   # coverage.exclude — generated code is not ours to test
   ^github.com/org/repo/gen/
   \.gen\.go:
   # the test harness itself
   /internal/testharness/
   # needs a private network nobody has in CI
   /internal/transport/overlay\.go:
   ```
   ```
   grep -v -E '^(#|$)' coverage.exclude > .cover/exclude
   { head -1 .cover/merged.out; tail -n +2 .cover/merged.out | grep -v -E -f .cover/exclude; } > .cover/cover.out
   ```
   A recipe that needs process substitution breaks under `sh`; write the
   temp file.
5. **Gate** on a committed baseline, half a point under the measurement:
   randomized, parallel specs jitter by 0.3 to 0.5 between runs.
   ```
   awk -v base="$(cat coverage.baseline)" 'FNR == 1 { next } { t += $2; if ($3 > 0) c += $2 }
     END { p = int(1000 * c / t) / 10; printf "cover: %.1f%% of %d hand-written statements (baseline %.1f%%)\n", p, t, base;
           if (p < base) { print "cover: below coverage.baseline"; exit 1 } }' .cover/cover.out
   ```

## Reading the gaps

Uncovered statements per file, largest first:
```
awk 'FNR==1{next}{split($1,a,":");f=a[1];T[f]+=$2;if($3==0)U[f]+=$2}END{for(f in U)printf "%4d/%4d %s\n",U[f],T[f],f}' .cover/cover.out | sort -rn | head -20
```
Then `go tool cover -html=.cover/cover.out` for the lines. Per gap, in this
order: is it dead (nothing calls it: delete) · is it reachable only with
infrastructure the suite cannot have (exclude, with the reason) · else what
behaviour would a user notice if this line were wrong (that is the spec's
name).

Always write a suite's full output to a log and grep it afterwards. A
filtered run loses the failing spec's name.
