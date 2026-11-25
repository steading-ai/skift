// Command mutatesql is mutation testing for the store's SQL, which
// gremlins cannot reach: it breaks each rule in the mutants table in turn,
// runs the store, domain and server specs, and reports whether a spec
// noticed. Run it from the repository root through make mutate-sql.
//
// It exits 0 when every mutant is killed, 1 when one lived or a run broke,
// and 2 when the table no longer matches the tree or a run could not be
// carried out.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(interruptible(func(ctx context.Context) int {
		return run(ctx, ".", mutants, ginkgo, os.Stdout, os.Stderr)
	}))
}

// interruptible runs fn with a context an interrupt or SIGTERM cancels,
// so the mutant in play is restored before the process exits.
func interruptible(fn func(context.Context) int) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return fn(ctx)
}

// run is main without the process: the exit code for running ms.
func run(ctx context.Context, root string, ms []mutant, suite Suite, stdout, stderr io.Writer) int {
	results, err := runAll(ctx, root, ms, suite, stdout)
	if err == nil {
		code, summary := 0, fmt.Sprintf("all %d mutants killed", len(results))
		if n := survived(results); n > 0 {
			code, summary = 1, fmt.Sprintf("%d of %d mutants survived", n, len(results))
		}
		if _, err = fmt.Fprintln(stdout, summary); err == nil {
			return code
		}
	}
	// stderr is the last place left to say why; a failure there has nowhere to go.
	_, _ = fmt.Fprintln(stderr, "mutate-sql:", err)
	return 2
}

// ginkgo runs the suites that exercise the store's SQL: the store's own,
// and the domain and server specs that reach it. It stops at the first
// failing spec, since one is enough to kill a mutant.
func ginkgo(ctx context.Context, root string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "tool", "ginkgo", "-p", "--fail-fast", "--no-color",
		"./internal/store", "./internal/domain", "./internal/server")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}
