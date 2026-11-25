package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("SQL mutation testing", Label("unit"), func() {
	var root string

	// tree lays out a repository with one store file and returns a mutant
	// of it.
	tree := func(content string) mutant {
		GinkgoHelper()
		root = GinkgoT().TempDir()
		Expect(os.MkdirAll(filepath.Join(root, "internal/store"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(root, "internal/store/ready.go"), []byte(content), 0o644)).To(Succeed())
		return mutant{ID: "M01", File: "internal/store/ready.go", Old: "status = 'open'", New: "true", Rule: "closed nodes count as open"}
	}
	read := func(rel string) string {
		GinkgoHelper()
		data, err := os.ReadFile(filepath.Join(root, rel))
		Expect(err).NotTo(HaveOccurred())
		return string(data)
	}
	// failing is a suite whose output carries a failing spec.
	failing := func(context.Context, string) (string, error) {
		return "Summarizing 1 Failure:\n  [FAIL] Readiness [It] excludes a closed node [db]\n", errors.New("exit status 1")
	}
	passing := func(context.Context, string) (string, error) { return "Test Suite Passed\n", nil }

	It("The table applies to the tree", func() {
		By("given the mutants table and this repository")
		By("then every mutant's old text occurs exactly once in its file")
		Expect(check("../../..", mutants)).To(Succeed(), "update the row in mutants.go to the SQL as it now reads")
	})

	It("A stale mutant fails the run before anything is mutated", func() {
		By("given a mutant whose old text no longer occurs once")
		m := tree("WHERE status = 'open' AND status = 'open'")
		stale := m
		stale.ID = "M02"
		stale.Old = "held_at IS NULL"

		By("when the mutants run")
		calls := 0
		suite := func(context.Context, string) (string, error) { calls++; return "", nil }
		_, err := runAll(context.Background(), root, []mutant{m, stale}, suite, &bytes.Buffer{})

		By("then the run fails naming each, and no suite ran")
		Expect(err).To(MatchError(And(
			ContainSubstring("M01: its old text occurs 2 times"),
			ContainSubstring("M02: its old text occurs 0 times"),
		)))
		Expect(calls).To(BeZero())
		Expect(read("internal/store/ready.go")).To(Equal("WHERE status = 'open' AND status = 'open'"))
	})

	It("names a mutant whose file is missing", func() {
		m := tree("WHERE status = 'open'")
		m.File = "internal/store/gone.go"
		Expect(check(root, []mutant{m})).To(MatchError(ContainSubstring("M01:")))
	})

	It("runs the suite against the mutated file, then restores it", func() {
		m := tree("WHERE status = 'open'")
		var seen string
		suite := func(_ context.Context, dir string) (string, error) {
			seen = read("internal/store/ready.go")
			Expect(dir).To(Equal(root))
			return failing(context.Background(), dir)
		}
		res, err := runOne(context.Background(), root, m, suite)
		Expect(err).NotTo(HaveOccurred())
		Expect(seen).To(Equal("WHERE true"))
		Expect(read("internal/store/ready.go")).To(Equal("WHERE status = 'open'"))
		Expect(res.Outcome).To(Equal(Killed))
		Expect(res.Failed).To(Equal("Readiness [It] excludes a closed node [db]"))
	})

	It("calls a mutant every spec passes with lived", func() {
		m := tree("WHERE status = 'open'")
		res, err := runOne(context.Background(), root, m, passing)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Outcome).To(Equal(Lived))
	})

	It("calls a failed run with no failing spec broken, not killed", func() {
		suite := func(context.Context, string) (string, error) {
			return "# github.com/steading-ai/skift/internal/store\nready.go:3:1: syntax error\n", errors.New("exit status 1")
		}
		m := tree("WHERE status = 'open'")
		res, err := runOne(context.Background(), root, m, suite)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Outcome).To(Equal(Broken))
		Expect(res.Tail).To(ContainSubstring("syntax error"))
	})

	It("An interrupted run restores the mutated file", func() {
		By("given a mutant whose suite is interrupted mid-run")
		m := tree("WHERE status = 'open'")
		ctx, cancel := context.WithCancel(context.Background())
		suite := func(context.Context, string) (string, error) {
			cancel()
			return "", context.Canceled
		}

		By("when it runs")
		_, err := runOne(ctx, root, m, suite)

		By("then the run stops with the interruption, and the file is as it was")
		Expect(err).To(MatchError(context.Canceled))
		Expect(read("internal/store/ready.go")).To(Equal("WHERE status = 'open'"))
	})

	It("removes the rapid failures a run saves, and keeps the ones already there", func() {
		m := tree("WHERE status = 'open'")
		kept := filepath.Join(root, rapidDir, "Derived/Derived-1.fail")
		Expect(os.MkdirAll(filepath.Dir(kept), 0o755)).To(Succeed())
		Expect(os.WriteFile(kept, []byte("saved"), 0o644)).To(Succeed())
		saved := filepath.Join(root, rapidDir, "Hold_summary/Hold_summary-2.fail")
		suite := func(ctx context.Context, dir string) (string, error) {
			Expect(os.MkdirAll(filepath.Dir(saved), 0o755)).To(Succeed())
			Expect(os.WriteFile(saved, []byte("new"), 0o644)).To(Succeed())
			return failing(ctx, dir)
		}
		_, err := runOne(context.Background(), root, m, suite)
		Expect(err).NotTo(HaveOccurred())
		Expect(kept).To(BeAnExistingFile())
		Expect(saved).NotTo(BeAnExistingFile())
		Expect(filepath.Dir(saved)).NotTo(BeADirectory())
	})

	It("reports each result as it goes", func() {
		m := tree("WHERE status = 'open'")
		var out bytes.Buffer
		results, err := runAll(context.Background(), root, []mutant{m}, failing, &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(1))
		Expect(out.String()).To(And(
			ContainSubstring("M01  killed"),
			ContainSubstring("closed nodes count as open"),
			ContainSubstring("by Readiness [It] excludes a closed node [db]"),
		))
	})

	Describe("the exit code", func() {
		It("is 0 when every mutant is killed", func() {
			m := tree("WHERE status = 'open'")
			var out bytes.Buffer
			Expect(run(context.Background(), root, []mutant{m}, failing, &out, &bytes.Buffer{})).To(Equal(0))
			Expect(out.String()).To(ContainSubstring("all 1 mutants killed"))
		})

		It("is 1 when a mutant lives", func() {
			m := tree("WHERE status = 'open'")
			var out bytes.Buffer
			Expect(run(context.Background(), root, []mutant{m}, passing, &out, &bytes.Buffer{})).To(Equal(1))
			Expect(out.String()).To(And(ContainSubstring("M01  LIVED"), ContainSubstring("1 of 1 mutants survived")))
		})

		It("is 2 when the table no longer matches the tree", func() {
			m := tree("WHERE held_at IS NULL")
			var errOut bytes.Buffer
			Expect(run(context.Background(), root, []mutant{m}, passing, &bytes.Buffer{}, &errOut)).To(Equal(2))
			Expect(errOut.String()).To(ContainSubstring("M01: its old text occurs 0 times"))
		})
	})

	It("fails a run whose report cannot be written", func() {
		m := tree("WHERE status = 'open'")
		var errOut bytes.Buffer
		Expect(run(context.Background(), root, []mutant{m}, failing, brokenWriter{}, &errOut)).To(Equal(2))
		Expect(errOut.String()).To(ContainSubstring("report M01: disk full"))
	})

	It("cancels its context on interrupt and returns what the run returns", func() {
		Expect(interruptible(func(ctx context.Context) int {
			Expect(ctx.Err()).NotTo(HaveOccurred())
			return 3
		})).To(Equal(3))
	})
})

// brokenWriter fails every write, like a report on a full disk.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
