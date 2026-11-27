package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// migrationsDir is where a repository keeps its migrations.
const migrationsDir = "internal/store/migrations"

// now is when every spec's check runs. testdata/skift.json is the
// promotion history as it stood then: v0.2.0 on stable for weeks but
// yanked, v0.3.0 on stable for eight days, v0.4.0 for three, v0.5.0 only
// on edge, and v0.1.0, which predates the history, absent.
var now = time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)

var _ = Describe("The floor policy check", Label("unit"), func() {
	// git runs git in root, failing the spec if it fails.
	git := func(root string, args ...string) {
		GinkgoHelper()
		cmd := exec.Command("git", append([]string{
			"-C", root,
			"-c", "user.name=spec", "-c", "user.email=spec@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
			"-c", "core.hooksPath=/dev/null", "-c", "maintenance.auto=false",
		}, args...)...)
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	}
	// migration writes migration v, with up and down as its sections.
	migration := func(root string, v int, up, down string) {
		GinkgoHelper()
		path := filepath.Join(root, migrationsDir, fmt.Sprintf("%05d_m%d.sql", v, v))
		Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
		Expect(os.WriteFile(path, []byte("-- +goose Up\n"+up+"\n\n-- +goose Down\n"+down+"\n"), 0o644)).To(Succeed())
	}
	additive := func(v int) (string, string) {
		return fmt.Sprintf("CREATE TABLE t%d (id bigint PRIMARY KEY);", v), fmt.Sprintf("DROP TABLE t%d;", v)
	}
	// released lays out a repository in which release v0.i.0 carries
	// migrations 1 to newest[i-1], and a candidate, v0.N.0-rc.1, carries one
	// more. ups replaces a migration's Up section.
	released := func(newest []int, ups map[int]string) string {
		GinkgoHelper()
		root := GinkgoT().TempDir()
		git(root, "init", "--quiet")
		written := 0
		for i, upTo := range append(slices.Clone(newest), newest[len(newest)-1]+1) {
			for ; written < upTo; written++ {
				up, down := additive(written + 1)
				if u, ok := ups[written+1]; ok {
					up = u
				}
				migration(root, written+1, up, down)
			}
			git(root, "add", "--all")
			git(root, "commit", "--quiet", "--allow-empty", "--message", fmt.Sprintf("migrations to %d", upTo))
			tag := fmt.Sprintf("v0.%d.0", i+1)
			if i == len(newest) {
				tag += "-rc.1"
			}
			git(root, "tag", tag)
		}
		return root
	}
	// oneEach is a release per migration: v0.1.0 to v0.5.0 carry 1 to 5,
	// and the candidate v0.6.0-rc.1 carries 6.
	oneEach := []int{1, 2, 3, 4, 5}
	// raising adds migration 7, not yet released, raising the floor to f.
	raising := func(root string, f int) {
		GinkgoHelper()
		migration(root, 7, fmt.Sprintf("ALTER TABLE t1 DROP COLUMN IF EXISTS gone;\nUPDATE schema_floor SET version = %d;", f), "UPDATE schema_floor SET version = 1;")
	}
	fixture := func(context.Context) ([]byte, error) { return os.ReadFile("testdata/skift.json") }
	// skip is whether the next check runs as though skipEnv were set.
	var skip bool
	BeforeEach(func() { skip = false })
	// check runs the check over every migration in root, as hk check --all
	// does, reading the history from history.
	check := func(root string, history func(context.Context) ([]byte, error)) (int, string) {
		GinkgoHelper()
		files, err := filepath.Glob(filepath.Join(root, migrationsDir, "*.sql"))
		Expect(err).NotTo(HaveOccurred())
		for i, f := range files {
			files[i], err = filepath.Rel(root, f)
			Expect(err).NotTo(HaveOccurred())
		}
		var out bytes.Buffer
		code := run(context.Background(), config{root: root, files: files, now: now, skip: skip, history: history}, &out)
		GinkgoWriter.Print(out.String())
		return code, out.String()
	}
	// unread is a history that counts its reads and has none to give.
	unread := func(reads *int) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) {
			*reads++
			return nil, errors.New("no network in this spec")
		}
	}

	It("Scenario: Raising the floor to a well-proven release", func() {
		By("given a migration that raises the floor to a release eight days on stable")
		root := released(oneEach, nil)
		raising(root, 3)

		By("when the floor policy check runs")
		code, out := check(root, fixture)

		By("then it passes")
		Expect(code).To(Equal(0), out)
		Expect(out).To(ContainSubstring("00007_m7.sql:3: raises the floor to migration 3, first carried by skift 0.3.0, which entered stable at 2026-12-02T00:00:00Z, 8.0 days ago"))
	})

	It("Scenario: Raising the floor too soon", func() {
		By("given a migration that raises the floor to a release three days on stable")
		root := released(oneEach, nil)
		raising(root, 4)

		By("when the floor policy check runs")
		code, out := check(root, fixture)

		By("then it fails")
		Expect(code).To(Equal(1))

		By("and it names the release and when it reached stable")
		Expect(out).To(ContainSubstring("internal/store/migrations/00007_m7.sql:3: raises the floor to migration 4, first carried by skift 0.4.0, which entered stable at 2026-12-07T00:00:00Z, 3.0 days ago, so not before 2026-12-14T00:00:00Z"))
	})

	DescribeTable("fails a raise to a release the rule does not allow, saying why",
		func(floor int, why string) {
			root := released(oneEach, nil)
			raising(root, floor)
			code, out := check(root, fixture)
			Expect(code).To(Equal(1))
			Expect(out).To(ContainSubstring(fmt.Sprintf("raises the floor to migration %d", floor)))
			Expect(out).To(ContainSubstring(why))
		},
		Entry("a yanked release, however long it was on stable", 2, "first carried by skift 0.2.0, which was yanked at 2026-11-20T00:00:00Z (loses holds on restart)"),
		Entry("a release only on edge", 5, "first carried by skift 0.5.0, which has not reached stable"),
		Entry("a release the history does not list", 1, "first carried by skift 0.1.0, which never reached edge"),
		Entry("a migration only a candidate carries", 6, "which no release carries yet: the newest, v0.5.0, carries 5"),
		Entry("a migration no release carries", 7, "which no release carries yet"),
	)

	It("names the oldest release whose newest migration meets the floor", func() {
		By("given releases in which v0.3.0 is the first to carry migration 3, and carries 4 beside it")
		root := released([]int{1, 2, 4, 5}, nil)

		By("when a new migration raises the floor to 3")
		raising(root, 3)
		code, out := check(root, fixture)

		By("then the release it names is v0.3.0, eight days on stable")
		Expect(code).To(Equal(0), out)
		Expect(out).To(ContainSubstring("00007_m7.sql:3: raises the floor to migration 3, first carried by skift 0.3.0"))
	})

	It("does not check a migration that leaves the floor alone, nor read the history", func() {
		By("given a new migration that only adds")
		root := released(oneEach, nil)
		up, down := additive(7)
		migration(root, 7, up, down)

		By("when the floor policy check runs with no network")
		reads := 0
		code, out := check(root, unread(&reads))

		By("then it passes without reading the history")
		Expect(code).To(Equal(0), out)
		Expect(reads).To(BeZero())
		Expect(out).To(BeEmpty())
	})

	It("does not check a migration the newest release already carries", func() {
		By("given the newest release shipped a migration that raised the floor to itself")
		root := released(oneEach, map[int]string{5: "UPDATE schema_floor SET version = 5;"})

		By("when the floor policy check runs with no network")
		reads := 0
		code, out := check(root, unread(&reads))

		By("then it passes without reading the history")
		Expect(code).To(Equal(0), out)
		Expect(reads).To(BeZero())
	})

	It("reads a raise only from the Up section, outside comments", func() {
		By("given a new migration that mentions a raise in a comment and puts the floor back in its Down section")
		root := released(oneEach, nil)
		migration(root, 7, "-- A later migration runs UPDATE schema_floor SET version = 7;\nCREATE TABLE t7 (id bigint PRIMARY KEY);", "UPDATE schema_floor SET version = 4;")

		By("when the floor policy check runs with no network")
		reads := 0
		code, out := check(root, unread(&reads))

		By("then it finds no raise")
		Expect(code).To(Equal(0), out)
		Expect(reads).To(BeZero())
	})

	It("reads a raise written across lines, in any case, on a qualified table", func() {
		root := released(oneEach, nil)
		migration(root, 7, "CREATE TABLE t7 (id bigint PRIMARY KEY);\nupdate public.schema_floor\n   set version=4;", "")
		code, out := check(root, fixture)
		Expect(code).To(Equal(1))
		Expect(out).To(ContainSubstring("00007_m7.sql:3: raises the floor to migration 4"))
	})

	It("checks every raise in every new migration", func() {
		root := released(oneEach, nil)
		raising(root, 3)
		migration(root, 8, "UPDATE schema_floor SET version = 4;", "UPDATE schema_floor SET version = 3;")
		code, out := check(root, fixture)
		Expect(code).To(Equal(1))
		Expect(out).To(ContainSubstring("00007_m7.sql:3: raises the floor to migration 3, first carried by skift 0.3.0, which entered stable"))
		Expect(out).To(ContainSubstring("00008_m8.sql:2: raises the floor to migration 4, first carried by skift 0.4.0, which entered stable at 2026-12-07T00:00:00Z"))
	})

	Describe("without the promotion history", func() {
		It("fails closed, saying it cannot confirm the rule", func() {
			By("given a migration that raises the floor")
			root := released(oneEach, nil)
			raising(root, 3)

			By("when the floor policy check runs offline")
			reads := 0
			code, out := check(root, unread(&reads))

			By("then it fails, saying it cannot confirm the rule and how to skip it")
			Expect(code).To(Equal(1))
			Expect(reads).To(Equal(1))
			Expect(out).To(SatisfyAll(
				ContainSubstring("raises the floor to migration 3, first carried by skift 0.3.0"),
				ContainSubstring("cannot confirm it has been seven days on stable"),
				ContainSubstring("no network in this spec"),
				ContainSubstring(skipEnv),
			))
		})

		It("fails closed on a history it cannot read", func() {
			root := released(oneEach, nil)
			raising(root, 3)
			code, out := check(root, func(context.Context) ([]byte, error) { return []byte("<html>"), nil })
			Expect(code).To(Equal(1))
			Expect(out).To(ContainSubstring("cannot confirm"))
		})

		It("skips reading it when the skip variable is set, saying what it did not confirm", func() {
			root := released(oneEach, nil)
			raising(root, 4)
			reads := 0
			skip = true
			code, out := check(root, unread(&reads))
			Expect(code).To(Equal(0), out)
			Expect(reads).To(BeZero())
			Expect(out).To(ContainSubstring("raises the floor to migration 4, first carried by skift 0.4.0; not confirmed on stable, since " + skipEnv + " is set"))
		})

		It("still fails a raise no release carries when skipping", func() {
			root := released(oneEach, nil)
			raising(root, 7)
			skip = true
			code, out := check(root, unread(new(int)))
			Expect(code).To(Equal(1))
			Expect(out).To(ContainSubstring("which no release carries yet"))
		})
	})

	It("passes a repository with no release tag, which has nothing to name", func() {
		root := GinkgoT().TempDir()
		git(root, "init", "--quiet")
		raising(root, 3)
		code, out := check(root, unread(new(int)))
		Expect(code).To(Equal(0), out)
		Expect(out).To(ContainSubstring("no release tag"))
	})

	It("CI never sets the skip variable", func() {
		By("given every workflow and the hk configuration")
		files, err := filepath.Glob("../../../.github/workflows/*.yml")
		Expect(err).NotTo(HaveOccurred())
		Expect(files).NotTo(BeEmpty())
		files = append(files, "../../../hk.pkl", "../../../Makefile")

		By("then none names it")
		for _, f := range files {
			data, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).NotTo(ContainSubstring(skipEnv), "%s names %s, which only local development sets", f, skipEnv)
		}
	})
})
