package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Outcome is what the specs made of one mutant.
type Outcome string

const (
	// Killed: a spec failed, so some spec holds the rule.
	Killed Outcome = "killed"
	// Lived: every spec passed with the rule broken.
	Lived Outcome = "LIVED"
	// Broken: the run failed without a failing spec, such as a build that
	// no longer compiles. It proves nothing about the rule.
	Broken Outcome = "BROKEN"
)

// Result is one mutant's run.
type Result struct {
	Mutant  mutant
	Outcome Outcome
	Elapsed time.Duration
	// Failed is the first failing spec's name, for a killed mutant.
	Failed string
	// Tail is the end of the output, for a broken run.
	Tail string
}

// Suite runs the specs in root and returns their combined output. err is
// nil when every spec passed.
type Suite func(ctx context.Context, root string) (output string, err error)

// rapidDir is where rapid saves the failing cases of a property. A mutant
// that kills a property leaves one there, and a later run would replay it.
const rapidDir = "internal/domain/testdata/rapid"

// check reports every mutant whose Old text does not occur exactly once in
// its file, so a stale row fails the run before anything is mutated.
func check(root string, ms []mutant) error {
	var errs []error
	for _, m := range ms {
		data, err := os.ReadFile(filepath.Join(root, m.File))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.ID, err))
			continue
		}
		if n := strings.Count(string(data), m.Old); n != 1 {
			errs = append(errs, fmt.Errorf("%s: its old text occurs %d times in %s, not once", m.ID, n, m.File))
		}
	}
	return errors.Join(errs...)
}

// runOne applies m, runs suite, and restores the file whatever happens,
// including when ctx is cancelled mid-run. Rapid failures the run saved
// are removed.
func runOne(ctx context.Context, root string, m mutant, suite Suite) (res Result, err error) {
	res.Mutant = m
	path := filepath.Join(root, m.File)
	orig, err := os.ReadFile(path)
	if err != nil {
		return res, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return res, err
	}
	before, err := listFiles(filepath.Join(root, rapidDir))
	if err != nil {
		return res, err
	}
	mutated := strings.Replace(string(orig), m.Old, m.New, 1)
	if err := os.WriteFile(path, []byte(mutated), info.Mode()); err != nil {
		return res, err
	}
	defer func() {
		if werr := os.WriteFile(path, orig, info.Mode()); werr != nil {
			err = errors.Join(err, fmt.Errorf("restore %s: %w", m.File, werr))
		}
		if rerr := removeNew(filepath.Join(root, rapidDir), before); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()

	start := time.Now()
	out, runErr := suite(ctx, root)
	res.Elapsed = time.Since(start)
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	res.Outcome, res.Failed, res.Tail = classify(out, runErr)
	return res, nil
}

// classify reads a run's output. Ginkgo marks each failing spec with a
// line starting [FAIL] in its summary.
func classify(out string, runErr error) (outcome Outcome, failed, tail string) {
	if runErr == nil {
		return Lived, "", ""
	}
	for line := range strings.Lines(out) {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "[FAIL]") {
			return Killed, strings.TrimSpace(strings.TrimPrefix(s, "[FAIL]")), ""
		}
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return Broken, "", strings.Join(lines[max(0, len(lines)-5):], "\n")
}

// runAll checks every mutant, then runs them one at a time, writing a line
// per result to w. It returns the results so far when ctx is cancelled.
func runAll(ctx context.Context, root string, ms []mutant, suite Suite, w io.Writer) ([]Result, error) {
	if err := check(root, ms); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(ms))
	for _, m := range ms {
		res, err := runOne(ctx, root, m, suite)
		if err != nil {
			return results, fmt.Errorf("%s: %w", m.ID, err)
		}
		results = append(results, res)
		if _, err := fmt.Fprintln(w, formatResult(res)); err != nil {
			return results, fmt.Errorf("report %s: %w", m.ID, err)
		}
	}
	return results, nil
}

// formatResult is one line of the report: id, outcome, seconds, the rule,
// and the spec that killed it or the end of a broken run.
func formatResult(r Result) string {
	line := fmt.Sprintf("%s  %-6s  %4.0fs  %s", r.Mutant.ID, r.Outcome, r.Elapsed.Seconds(), r.Mutant.Rule)
	switch r.Outcome {
	case Killed:
		line += "\n      by " + r.Failed
	case Broken:
		line += "\n      " + strings.ReplaceAll(r.Tail, "\n", "\n      ")
	}
	return line
}

// survived counts the mutants that are not killed: lived or broken.
func survived(results []Result) int {
	n := 0
	for _, r := range results {
		if r.Outcome != Killed {
			n++
		}
	}
	return n
}

// listFiles is every file under dir, by path; empty when dir is absent.
func listFiles(dir string) (map[string]bool, error) {
	files := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files[path] = true
		}
		return nil
	})
	return files, err
}

// removeNew deletes every file under dir that is not in before, and any
// directory that leaves empty.
func removeNew(dir string, before map[string]bool) error {
	after, err := listFiles(dir)
	if err != nil {
		return err
	}
	for path := range after {
		if before[path] {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if parent := filepath.Dir(path); parent != dir {
			if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
				if err := os.Remove(parent); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
