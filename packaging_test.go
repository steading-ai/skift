package dagger_test

import (
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// runIn executes a command in dir and returns what it wrote to stdout once
// it has exited successfully. Git's own variables are dropped from the
// environment: under a pre-commit hook they point every git command, and
// the Makefile's git describe, at the repository being committed to rather
// than the copy.
func runIn(dir string, timeout time.Duration, name string, args ...string) string {
	GinkgoHelper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Expect(session.Wait(timeout)).To(gexec.Exit(0))
	return strings.TrimSpace(string(session.Out.Contents()))
}

// git runs a git command in a copied checkout with a fixed identity, so a
// commit needs no configuration from the machine running the specs, and
// without hooks, so a global pre-commit hook does not lint the copy.
func git(dir string, args ...string) {
	GinkgoHelper()
	runIn(dir, time.Minute, "git", append([]string{
		"-c", "user.name=spec", "-c", "user.email=spec@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
	}, args...)...)
}

// makeVersion is what `make version` prints in the given checkout, which
// is the version the next build there would carry.
func makeVersion(dir string) string {
	GinkgoHelper()
	return runIn(dir, time.Minute, "make", "version")
}

var _ = Describe("Packaging", Label("unit", "slow"), func() {
	BeforeEach(func() {
		if os.Getenv(nestedRun) != "" {
			Skip("already inside make test-unit")
		}
	})

	Describe("the version", func() {
		var dir string

		// The copy has no history of its own, so the spec gives it one:
		// one commit, tagged, and clean.
		BeforeEach(func() {
			dir = checkout()
			git(dir, "init", "-q")
			git(dir, "add", "-A")
			git(dir, "commit", "-q", "--no-verify", "-m", "initial")
			git(dir, "tag", "v9.9.9")
		})

		It("is the tag on a clean tagged checkout", func() {
			Expect(makeVersion(dir)).To(Equal("9.9.9"))
		})

		It("carries a development marker when the tree is dirty", func() {
			appendToFile(dir, "README.md", "\nedited\n")
			Expect(makeVersion(dir)).To(MatchRegexp(`^9\.9\.9\+dev\.0\.g[0-9a-f]+\.dirty$`))
		})

		It("carries a development marker when past the tag", func() {
			appendToFile(dir, "README.md", "\nedited\n")
			git(dir, "commit", "-q", "--no-verify", "-am", "edit")
			Expect(makeVersion(dir)).To(MatchRegexp(`^9\.9\.9\+dev\.1\.g[0-9a-f]+$`))
		})

		It("is semver-shaped with no tag at all", func() {
			git(dir, "tag", "-d", "v9.9.9")
			Expect(makeVersion(dir)).To(MatchRegexp(`^0\.0\.0\+dev\.1\.g[0-9a-f]+$`))
		})
	})
})
