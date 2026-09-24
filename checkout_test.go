package dagger_test

import (
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

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
