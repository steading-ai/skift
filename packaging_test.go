package skift_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cavaliergopher/rpm"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// runIn executes a command in dir and returns what it wrote to stdout once
// it has exited successfully. Git's and make's own variables are dropped
// from the environment. Under a pre-commit hook the git ones point every
// git command, and the Makefile's git describe, at the repository being
// committed to rather than the copy. Under make test-unit the make ones
// mark the copy's make as a sub-make, and a sub-make announces directory
// changes on stdout, in front of the value the spec reads.
func runIn(dir string, timeout time.Duration, name string, args ...string) string {
	GinkgoHelper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if inherited(kv) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Expect(session.Wait(timeout)).To(gexec.Exit(0))
	return strings.TrimSpace(string(session.Out.Contents()))
}

// inherited reports whether an environment entry passes through to a
// command run in a copied checkout.
func inherited(kv string) bool {
	for _, prefix := range []string{"GIT_", "MAKE", "MFLAGS=", "GNUMAKEFLAGS="} {
		if strings.HasPrefix(kv, prefix) {
			return false
		}
	}
	return true
}

// git runs a git command in a copied checkout with a fixed identity, so a
// commit needs no configuration from the machine running the specs; with
// no hooks at all, since --no-verify skips only pre-commit and commit-msg
// and a machine-wide hooks path would run the rest; and with no automatic
// maintenance, which a commit otherwise starts detached, repacking the
// copy while the spec's cleanup removes it.
func git(dir string, args ...string) {
	GinkgoHelper()
	runIn(dir, time.Minute, "git", append([]string{
		"-c", "user.name=spec", "-c", "user.email=spec@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		"-c", "core.hooksPath=/dev/null", "-c", "maintenance.auto=false",
	}, args...)...)
}

// makeVersion is what `make version` prints in the given checkout, which
// is the version the next build there would carry.
func makeVersion(dir string) string {
	GinkgoHelper()
	return runIn(dir, time.Minute, "make", "version")
}

// rpmPaths lists the paths a package would install, as its header records
// them.
func rpmPaths(path string) []string {
	GinkgoHelper()
	pkg, err := rpm.Open(path)
	Expect(err).NotTo(HaveOccurred())
	paths := make([]string, 0, len(pkg.Files()))
	for _, f := range pkg.Files() {
		paths = append(paths, f.Name())
	}
	return paths
}

var _ = Describe("Packaging", Label("unit", "slow"), func() {
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

	Describe("the rpm", Ordered, func() {
		var dir string

		// One package build serves every spec below. Two cross-compiles of
		// the binary, from the shared build cache.
		BeforeAll(func() {
			dir = checkout()
			runIn(dir, 10*time.Minute, "make", "package", "VERSION=9.9.9")
		})

		installed := []string{
			"/usr/bin/skift",
			"/usr/lib/systemd/system/skift.service",
			"/usr/lib/systemd/system/skift-reap.service",
			"/usr/lib/systemd/system/skift-reap.timer",
			"/usr/lib/sysusers.d/skift.conf",
			"/usr/lib/tmpfiles.d/skift.conf",
			"/etc/skift/env",
		}

		It("is built for both architectures with the units and fragments", func() {
			for _, rpm := range []string{"skift-9.9.9-1.x86_64.rpm", "skift-9.9.9-1.aarch64.rpm"} {
				Expect(rpmPaths(filepath.Join(dir, "dist", rpm))).To(ContainElements(installed), rpm)
			}
		})

		It("runs the service as the skift system user, not a login account", func() {
			unit, err := os.ReadFile(filepath.Join(dir, "contrib/systemd/skift.service"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(unit)).To(MatchRegexp(`(?m)^User=skift$`))
			Expect(string(unit)).To(MatchRegexp(`(?m)^WantedBy=multi-user\.target$`))
			Expect(string(unit)).NotTo(ContainSubstring("--tailnet"), "the packaged unit carries no tailnet options; the environment file turns the tailnet on")
			Expect(string(unit)).To(MatchRegexp(`(?m)^StateDirectory=skift$`), "the tailnet node's state outlives a restart")

			sysusers, err := os.ReadFile(filepath.Join(dir, "packaging/sysusers.d/skift.conf"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(sysusers)).To(MatchRegexp(`(?m)^u skift - `))
		})

		It("declares the socket directory in the skift group, setgid, mode 2750", func() {
			tmpfiles, err := os.ReadFile(filepath.Join(dir, "packaging/tmpfiles.d/skift.conf"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(tmpfiles)).To(MatchRegexp(`(?m)^d /run/skift 2750 skift skift -$`))
		})

		It("does not start the service from a scriptlet on first install", func() {
			for _, script := range []string{"preinstall.sh", "postinstall.sh"} {
				body, err := os.ReadFile(filepath.Join(dir, "packaging/scripts", script))
				Expect(err).NotTo(HaveOccurred())
				Expect(string(body)).NotTo(MatchRegexp(`systemctl\b.*\b(start|restart|enable|--now)\b`), script)
			}
		})

		It("stops and disables the units when the package is removed", func() {
			body, err := os.ReadFile(filepath.Join(dir, "packaging/scripts/preremove.sh"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(body)).To(ContainSubstring("disable --now skift.service skift-reap.timer"))
		})
	})
})

// The GHCR image built from packaging/Containerfile, published by
// .github/workflows/publish.yml. Needs podman to build and run, like the
// database specs, so it carries their "db" label and stays out of
// `make test-unit`.
var _ = Describe("The image", Label("db", "slow"), Ordered, func() {
	var dir, image string

	BeforeAll(func() {
		dir = checkout()
		// The same budget the rpm's build has: under the race detector the
		// build cache holds nothing this compile can use, and it runs
		// beside every other spec in the suite.
		runIn(dir, 10*time.Minute, "make", "dist/skift-linux-"+runtime.GOARCH, "VERSION=9.9.9")

		// Scoped to the checkout so two parallel Ginkgo processes building
		// this spec at once don't collide on the same image tag.
		image = "skift-image-test-" + filepath.Base(dir)
		runIn(dir, 5*time.Minute, "podman", "build",
			"--build-arg", "TARGETARCH="+runtime.GOARCH,
			"--build-arg", "REVISION=deadbeef",
			"-f", "packaging/Containerfile",
			"-t", image, ".")
		DeferCleanup(func() {
			runIn(dir, time.Minute, "podman", "rmi", "-f", image)
		})
	})

	It("reports the version the Makefile stamped it with", func() {
		Expect(runIn(dir, time.Minute, "podman", "run", "--rm", image, "skift", "version")).
			To(Equal("skift version 9.9.9"))
	})

	It("carries the revision as an OCI label", func() {
		Expect(runIn(dir, time.Minute, "podman", "inspect", "--format",
			`{{ index .Config.Labels "org.opencontainers.image.revision" }}`, image)).
			To(Equal("deadbeef"))
	})
})
