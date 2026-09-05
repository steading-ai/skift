package dagger_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// nestedRun marks a `make test-unit` launched from inside a spec, so the
// suite it starts does not start another.
const nestedRun = "DAGGER_SPEC_NESTED"

// checkout copies the working tree, tracked and untracked files alike, into
// a fresh directory. Ginkgo compiles each suite to a binary beside its
// package and removes it afterwards, so two runs over one tree would race
// on those files.
func checkout() string {
	dir := GinkgoT().TempDir()
	cmd := exec.Command("sh", "-euc",
		`git ls-files -z --cached --others --exclude-standard | tar --null --files-from - -cf - | tar -C "$0" -xf -`, dir)
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Expect(session.Wait(time.Minute)).To(gexec.Exit(0))
	trustConfig(filepath.Join(dir, "mise.toml"))
	return dir
}

// trustConfig lets mise read the copied mise.toml at its new path, so the
// tools make invokes in the copy resolve to the pinned versions. Trust is
// per path; it is withdrawn when the copy is removed.
func trustConfig(path string) {
	GinkgoHelper()
	run := func(args ...string) {
		session, err := gexec.Start(exec.Command("mise", args...), GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Wait(time.Minute)).To(gexec.Exit(0))
	}
	run("trust", "--quiet", path)
	DeferCleanup(run, "trust", "--quiet", "--untrust", path)
}

var _ = Describe("Unit tests", Label("unit", "slow"), func() {
	It("Unit tests need no container", func() {
		if os.Getenv(nestedRun) != "" {
			Skip("already inside make test-unit")
		}

		By("given podman is not running")
		// A socket nobody listens on stands in for a stopped podman: any
		// attempt to start a container fails immediately.
		env := append(os.Environ(),
			nestedRun+"=1",
			"DOCKER_HOST=unix:///nonexistent/podman.sock",
			"TESTCONTAINERS_RYUK_DISABLED=true",
		)

		cmd := exec.Command("make", "test-unit")
		cmd.Dir = checkout()
		cmd.Env = env
		session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		// Every suite compiles from the shared build cache, so this is
		// generous even for a cold one.
		Expect(session.Wait(5 * time.Minute)).To(gexec.Exit(0))
	})
})
