package skift_test

import (
	"archive/tar"
	"errors"
	"io"
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

	"github.com/steading-ai/skift/internal/testsupport"
)

// sessionIn executes a command in dir and returns it once it has exited,
// whatever its status. Git's and make's own variables are dropped from the
// environment. Under a pre-commit hook the git ones point every git
// command, and the Makefile's git describe, at the repository being
// committed to rather than the copy. Under make test-unit the make ones
// mark the copy's make as a sub-make, and a sub-make announces directory
// changes on stdout, in front of the value the spec reads.
func sessionIn(dir string, timeout time.Duration, name string, args ...string) *gexec.Session {
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
	return session.Wait(timeout)
}

// runIn executes a command in dir, as sessionIn does, and returns what it
// wrote to stdout once it has exited successfully.
func runIn(dir string, timeout time.Duration, name string, args ...string) string {
	GinkgoHelper()
	session := sessionIn(dir, timeout, name, args...)
	Expect(session).To(gexec.Exit(0))
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

// imageTree is every path in an image's filesystem with its tar header,
// read from a container created from the image and never started.
func imageTree(dir, image string) map[string]*tar.Header {
	GinkgoHelper()
	container := runIn(dir, time.Minute, "podman", "create", image, "skift", "version")
	DeferCleanup(func() {
		runIn(dir, time.Minute, "podman", "rm", "-f", container)
	})
	archive := filepath.Join(GinkgoT().TempDir(), "image.tar")
	runIn(dir, time.Minute, "podman", "export", "-o", archive, container)

	f, err := os.Open(archive)
	Expect(err).NotTo(HaveOccurred())
	defer func() { Expect(f.Close()).To(Succeed()) }()

	tree := map[string]*tar.Header{}
	for r := tar.NewReader(f); ; {
		header, err := r.Next()
		if errors.Is(err, io.EOF) {
			return tree
		}
		Expect(err).NotTo(HaveOccurred())
		tree[strings.TrimSuffix(header.Name, "/")] = header
	}
}

// containerStat is the owner and type of a path inside a running
// container, as "uid:gid type" in the container's own numbering, or the
// reason it could not be read. The image holds no stat to exec, so the
// container's filesystem is mounted and read from outside. Rootless podman
// mounts it only inside its user namespace, which is also where the ids
// read as the container's.
func containerStat(container, path string) (string, error) {
	const script = `root=$(podman mount "$0") && stat -c '%u:%g %F' "$root$1"`
	args := []string{"sh", "-c", script, container, path}
	if os.Geteuid() != 0 {
		args = append([]string{"podman", "unshare"}, args...)
	}
	session := run(os.Environ(), args[0], args[1:]...)
	if session.ExitCode() != 0 {
		return "", errors.New(strings.TrimSpace(string(session.Err.Contents())))
	}
	return strings.TrimSpace(string(session.Out.Contents())), nil
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

		// A release candidate's tag has a hyphen of its own, which is not
		// the one git describe adds.
		It("keeps a prerelease tag whole", func() {
			git(dir, "tag", "-d", "v9.9.9")
			git(dir, "tag", "v9.9.9-rc.1")
			Expect(makeVersion(dir)).To(Equal("9.9.9-rc.1"))

			appendToFile(dir, "README.md", "\nedited\n")
			git(dir, "commit", "-q", "--no-verify", "-am", "edit")
			Expect(makeVersion(dir)).To(MatchRegexp(`^9\.9\.9-rc\.1\+dev\.1\.g[0-9a-f]+$`))
		})

		It("is semver-shaped with no tag at all", func() {
			git(dir, "tag", "-d", "v9.9.9")
			Expect(makeVersion(dir)).To(MatchRegexp(`^0\.0\.0\+dev\.1\.g[0-9a-f]+$`))
		})
	})

	// What `make image-tags` prints is what the release workflow tags the
	// image with. docs/adrs/011-releases-are-immutable-and-attested.md,
	// decision 6, fixes the rule.
	Describe("the image tags", func() {
		var dir string

		// The tags a release owns depend on the other versions tagged, so
		// each spec tags a copy of its own. One commit carries every tag:
		// a version's place in history is not what orders it.
		BeforeEach(func() {
			dir = checkout()
			git(dir, "init", "-q")
			git(dir, "add", "-A")
			git(dir, "commit", "-q", "--no-verify", "-m", "initial")
		})

		tagged := func(tags ...string) {
			GinkgoHelper()
			for _, tag := range tags {
				git(dir, "tag", tag)
			}
		}

		// imageTags is what `make image-tags` prints for a version, one
		// tag per line.
		imageTags := func(version string) []string {
			GinkgoHelper()
			return strings.Split(runIn(dir, time.Minute, "make", "image-tags", "VERSION="+version), "\n")
		}

		It("The first stable release takes the moving tags", func() {
			By("given a repository whose only version tag is v0.1.0")
			tagged("v0.1.0")

			By("when make image-tags runs with VERSION=0.1.0")
			tags := imageTags("0.1.0")

			By("then it prints 0.1.0, 0.1 and latest")
			Expect(tags).To(Equal([]string{"0.1.0", "0.1", "latest"}))
			By("and it does not print 0")
			Expect(tags).NotTo(ContainElement("0"))
		})

		It("A prerelease owns only its exact tag", func() {
			By("given a repository tagged v0.1.0 and v0.2.0-rc.1")
			tagged("v0.1.0", "v0.2.0-rc.1")

			By("when make image-tags runs with VERSION=0.2.0-rc.1")
			tags := imageTags("0.2.0-rc.1")

			By("then it prints 0.2.0-rc.1 and nothing else")
			Expect(tags).To(Equal([]string{"0.2.0-rc.1"}))
		})

		It("A patch to an older line does not take latest", func() {
			By("given a repository tagged v1.2.3, v1.3.0 and v1.2.4")
			tagged("v1.2.3", "v1.3.0", "v1.2.4")

			By("when make image-tags runs with VERSION=1.2.4")
			tags := imageTags("1.2.4")

			By("then it prints 1.2.4 and 1.2")
			Expect(tags).To(Equal([]string{"1.2.4", "1.2"}))
			By("and it prints neither 1 nor latest")
			Expect(tags).NotTo(ContainElement("1"))
			Expect(tags).NotTo(ContainElement("latest"))
		})

		It("A release past 1.0 takes its major tag", func() {
			By("given a repository tagged v0.9.0 and v1.0.0")
			tagged("v0.9.0", "v1.0.0")

			By("when make image-tags runs with VERSION=1.0.0")
			tags := imageTags("1.0.0")

			By("then it prints 1.0.0, 1.0, 1 and latest")
			Expect(tags).To(Equal([]string{"1.0.0", "1.0", "1", "latest"}))
		})

		It("A development build owns no tags", func() {
			By("given a checkout three commits past v0.1.0")
			tagged("v0.1.0")
			for _, message := range []string{"one", "two", "three"} {
				git(dir, "commit", "-q", "--no-verify", "--allow-empty", "-m", message)
			}
			Expect(makeVersion(dir)).To(MatchRegexp(`^0\.1\.0\+dev\.3\.g[0-9a-f]+$`))

			By("when make image-tags runs with that checkout's version")
			session := sessionIn(dir, time.Minute, "make", "image-tags")

			By("then it exits non-zero")
			Expect(session).NotTo(gexec.Exit(0))
			By("and prints nothing on stdout")
			Expect(session.Out.Contents()).To(BeEmpty())
		})

		It("never counts a prerelease tag as a later version", func() {
			tagged("v0.1.0", "v0.2.0-rc.1")
			Expect(imageTags("0.1.0")).To(Equal([]string{"0.1.0", "0.1", "latest"}))
		})

		// Versions order by number, not by spelling: 1.10 is after 1.9.
		It("moves a tag forward past a two-digit version, and not back before one", func() {
			tagged("v1.9.0", "v1.10.0", "v1.10.1")
			Expect(imageTags("1.9.1")).To(Equal([]string{"1.9.1", "1.9"}))
			Expect(imageTags("1.10.1")).To(Equal([]string{"1.10.1", "1.10", "1", "latest"}))
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

		It("declares the ISC license in its header", func() {
			for _, name := range []string{"skift-9.9.9-1.x86_64.rpm", "skift-9.9.9-1.aarch64.rpm"} {
				pkg, err := rpm.Open(filepath.Join(dir, "dist", name))
				Expect(err).NotTo(HaveOccurred())
				Expect(pkg.License()).To(Equal("ISC"), name)
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
// `make test-unit`. Serial, because it compiles skift and builds an image:
// beside the suite's other specs that starves the ones that run `make
// lint` against a budget.
var _ = Describe("The image", Label("db", "slow"), Serial, Ordered, func() {
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

	It("runs as uid and gid 65532, with no entrypoint and no command", func() {
		Expect(runIn(dir, time.Minute, "podman", "inspect", "--format",
			`{{ .Config.User }} {{ len .Config.Entrypoint }} {{ len .Config.Cmd }}`, image)).
			To(Equal("65532:65532 0 0"))
	})

	It("gives skift an empty socket directory and state directory of its own, mode 0750", func() {
		tree := imageTree(dir, image)

		for _, path := range []string{"run/skift", "var/lib/skift"} {
			Expect(tree).To(HaveKey(path))
			header := tree[path]
			Expect(header.Typeflag).To(BeEquivalentTo(tar.TypeDir), path)
			Expect(header.Uid).To(Equal(65532), path)
			Expect(header.Gid).To(Equal(65532), path)
			Expect(header.Mode&0o7777).To(BeEquivalentTo(0o750), path)

			for name := range tree {
				Expect(name).NotTo(HavePrefix(path+"/"), "the directory is empty")
			}
		}
		for _, path := range []string{"run", "var", "var/lib"} {
			Expect(tree).To(HaveKey(path))
			Expect(tree[path].Uid).To(BeZero(), path)
			Expect(tree[path].Mode&0o7777).To(BeEquivalentTo(0o755), path)
		}
	})

	It("The image serves as an unprivileged user", func(ctx SpecContext) {
		By("given the image built from packaging/Containerfile, and a PostgreSQL the container can reach")
		postgres, err := testsupport.Start(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) error { return postgres.Stop(ctx) })
		databaseURL := postgres.EmptyDatabase(ctx)

		By("when the container runs skift serve with that database URL")
		// The host's network, so the URL that reaches PostgreSQL from here
		// reaches it from the container too.
		container := image + "-serve"
		runIn(dir, time.Minute, "podman", "run", "--detach", "--name", container, "--network", "host",
			image, "skift", "serve", "--database-url", databaseURL)
		DeferCleanup(func() {
			runIn(dir, time.Minute, "podman", "rm", "-f", container)
		})

		By(`then its log contains "skift serve starting"`)
		Eventually(func() string {
			logs := run(os.Environ(), "podman", "logs", container)
			return string(logs.Out.Contents()) + string(logs.Err.Contents())
		}).WithTimeout(time.Minute).WithPolling(250 * time.Millisecond).
			Should(ContainSubstring("skift serve starting"))

		By("and /run/skift/skift.sock exists inside the container, owned by uid 65532")
		Eventually(func() (string, error) {
			return containerStat(container, "/run/skift/skift.sock")
		}).WithTimeout(time.Minute).WithPolling(250 * time.Millisecond).
			Should(Equal("65532:65532 socket"))
	})

	It("The binary carries its own trust roots", func() {
		By("given the cmd/skift package, when its dependencies are listed")
		deps := strings.Fields(runIn(dir, time.Minute, "go", "list", "-deps", "./cmd/skift"))

		By("then golang.org/x/crypto/x509roots/fallback is among them")
		Expect(deps).To(ContainElement("golang.org/x/crypto/x509roots/fallback"))
	})
})
