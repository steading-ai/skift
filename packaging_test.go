package dagger_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// rpmPaths lists the paths a package would install, through libarchive's
// reading of the rpm payload.
func rpmPaths(rpm string) []string {
	GinkgoHelper()
	return strings.Split(runIn(filepath.Dir(rpm), time.Minute, "bsdtar", "-tf", rpm), "\n")
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

	Describe("the rpm", Ordered, func() {
		var dir string

		// One package build serves every spec below. Two cross-compiles of
		// the binary, from the shared build cache.
		BeforeAll(func() {
			dir = checkout()
			runIn(dir, 10*time.Minute, "make", "package", "VERSION=9.9.9")
		})

		installed := []string{
			"/usr/bin/dg",
			"/usr/lib/systemd/system/dagger.service",
			"/usr/lib/systemd/system/dagger-reap.service",
			"/usr/lib/systemd/system/dagger-reap.timer",
			"/usr/lib/sysusers.d/dagger.conf",
			"/usr/lib/tmpfiles.d/dagger.conf",
			"/etc/dagger/env",
		}

		It("is built for both architectures with the units and fragments", func() {
			for _, rpm := range []string{"dagger-9.9.9-1.x86_64.rpm", "dagger-9.9.9-1.aarch64.rpm"} {
				Expect(rpmPaths(filepath.Join(dir, "dist", rpm))).To(ContainElements(installed), rpm)
			}
		})

		It("runs the service as the dagger system user, not a login account", func() {
			unit, err := os.ReadFile(filepath.Join(dir, "contrib/systemd/dagger.service"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(unit)).To(MatchRegexp(`(?m)^User=dagger$`))
			Expect(string(unit)).To(MatchRegexp(`(?m)^WantedBy=multi-user\.target$`))
			Expect(string(unit)).NotTo(ContainSubstring("tailnet"), "the packaged unit carries no tailnet options")

			sysusers, err := os.ReadFile(filepath.Join(dir, "packaging/sysusers.d/dagger.conf"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(sysusers)).To(MatchRegexp(`(?m)^u dagger - `))
		})

		It("declares the socket directory in the dagger group, setgid, mode 2750", func() {
			tmpfiles, err := os.ReadFile(filepath.Join(dir, "packaging/tmpfiles.d/dagger.conf"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(tmpfiles)).To(MatchRegexp(`(?m)^d /run/dagger 2750 dagger dagger -$`))
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
			Expect(string(body)).To(ContainSubstring("disable --now dagger.service dagger-reap.timer"))
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
		if os.Getenv(nestedRun) != "" {
			Skip("already inside make test-unit")
		}

		dir = checkout()
		runIn(dir, 5*time.Minute, "make", "dist/dg-linux-"+runtime.GOARCH, "VERSION=9.9.9")

		// Scoped to the checkout so two parallel Ginkgo processes building
		// this spec at once don't collide on the same image tag.
		image = "dagger-image-test-" + filepath.Base(dir)
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
		Expect(runIn(dir, time.Minute, "podman", "run", "--rm", image, "dg", "version")).
			To(Equal("dg version 9.9.9"))
	})

	It("carries the revision as an OCI label", func() {
		Expect(runIn(dir, time.Minute, "podman", "inspect", "--format",
			`{{ index .Config.Labels "org.opencontainers.image.revision" }}`, image)).
			To(Equal("deadbeef"))
	})
})
