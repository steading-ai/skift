package skift_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cavaliergopher/rpm"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// fedoraImage is the Fedora the rpm is installed on to see what dnf does
// with it. renovate.json5 keeps it current.
const fedoraImage = "quay.io/fedora/fedora:45@sha256:1bd80cd53c142fe07d25dac925494e02fa7aec33534ea30440e74f85014359a1"

// configRelease is the release whose package installed /etc/skift/env as
// %config(noreplace), carrying the template itself. An upgrade from it
// must leave the operator's file as it found it.
const configRelease = "v0.4.0"

// rpmIn is the one package for this machine's architecture with the given
// version in a dist directory.
func rpmIn(dist, version string) string {
	GinkgoHelper()
	found, err := filepath.Glob(filepath.Join(dist, "skift-"+version+"-1.*.rpm"))
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(HaveLen(1))
	return found[0]
}

// fedoraBase builds Fedora with the package's dependencies, so a spec
// installs skift from its own files with every repository off, and
// returns the image's name. The image is removed when the spec's container
// ends.
func fedoraBase(dir string) string {
	GinkgoHelper()
	base := specName("fedora")
	builder := specName("fedora-build")
	runIn(dir, 10*time.Minute, "podman", "run", "--name", builder, fedoraImage, "dnf", "-y", "-q", "install", "systemd", "util-linux")
	DeferCleanup(func() {
		runIn(dir, time.Minute, "podman", "rm", "--force", builder)
	})
	runIn(dir, time.Minute, "podman", "commit", "--quiet", builder, base)
	DeferCleanup(func() {
		runIn(dir, time.Minute, "podman", "rmi", "--force", base)
	})
	return base
}

// The database a spec's Fedora reaches, from fedoraBesideDatabase. The
// password holds what a shell would expand, so a script that ran
// /etc/skift/env, rather than reading it, would not reach the database.
const (
	podDatabasePassword = `s3cret$HOME`
	podDatabaseURL      = "postgres://skift:" + podDatabasePassword + "@127.0.0.1:5432/skift?sslmode=disable"
)

// fedoraBesideDatabase starts a container of base running command, and a
// PostgreSQL beside it in one pod, the one compose.yaml runs, answering at
// podDatabaseURL with nothing in it yet. Both are removed when the spec
// ends. It returns their names.
func fedoraBesideDatabase(dir, base string, command ...string) (fedora, db string) {
	GinkgoHelper()
	pod := specName("pod")
	runIn(dir, time.Minute, "podman", "pod", "create", "--name", pod)
	DeferCleanup(func() {
		runIn(dir, time.Minute, "podman", "pod", "rm", "--force", "--time", "0", pod)
	})
	db, fedora = pod+"-db", pod+"-fedora"
	postgres := pinnedIn("compose.yaml", `(?m)^\s+image: (\S*/postgres:\S+)$`)
	runIn(dir, 5*time.Minute, "podman", "run", "--detach", "--pod", pod, "--name", db,
		"--env", "POSTGRES_USER=skift", "--env", "POSTGRES_PASSWORD="+podDatabasePassword, "--env", "POSTGRES_DB=skift", postgres)
	runIn(dir, time.Minute, "podman", append([]string{"run", "--detach", "--pod", pod, "--name", fedora, base}, command...)...)
	Eventually(func() *gexec.Session {
		return sessionIn(dir, time.Minute, "podman", "exec", db, "pg_isready", "--quiet", "--host", "127.0.0.1", "--username", "skift")
	}).WithTimeout(time.Minute).WithPolling(time.Second).Should(gexec.Exit(0))
	return fedora, db
}

// The rpm installed and upgraded by dnf on Fedora. Needs podman, like the
// database specs, so it carries their "db" label and stays out of `make
// test-unit`. Serial, as the image's spec is: it compiles skift and runs
// containers.
var _ = Describe("The rpm on Fedora", Label("db", "slow"), Serial, Ordered, func() {
	var dir, base, released, next, later string

	BeforeAll(func() {
		// Two packages from this tree whose templates differ by a comment,
		// as two releases' might.
		dir = checkout()
		runIn(dir, 10*time.Minute, "make", "package", "VERSION=9.9.8", "PACKAGE_ARCHES="+runtime.GOARCH)
		next = rpmIn(filepath.Join(dir, "dist"), "9.9.8")
		binary := filepath.Join(dir, "dist", "skift-linux-"+runtime.GOARCH)

		// The released package as its own packaging built it. What the
		// binary does is not in question, so it is this tree's.
		old := GinkgoT().TempDir()
		archive := filepath.Join(GinkgoT().TempDir(), "release.tar")
		runIn(".", time.Minute, "git", "archive", "--format=tar", "-o", archive, configRelease, "mise.toml", "packaging", "contrib")
		runIn(old, time.Minute, "tar", "-xf", archive)
		trustConfig(filepath.Join(old, "mise.toml"))
		raw, err := os.ReadFile(binary)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.MkdirAll(filepath.Join(old, "dist"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(old, "dist", "skift-linux-"+runtime.GOARCH), raw, 0o755)).To(Succeed())
		runIn(old, time.Minute, "env", "GOARCH="+runtime.GOARCH, "VERSION=0.4.0",
			"nfpm", "package", "--config", "packaging/nfpm.yaml", "--packager", "rpm", "--target", "dist/")
		released = rpmIn(filepath.Join(old, "dist"), "0.4.0")

		appendToFile(dir, "packaging/env", "\n# A comment a later release adds.\n")
		runIn(dir, 10*time.Minute, "make", "package", "VERSION=9.9.9", "PACKAGE_ARCHES="+runtime.GOARCH)
		later = rpmIn(filepath.Join(dir, "dist"), "9.9.9")

		base = fedoraBase(dir)
	})

	// host starts a container that has never had skift installed, beside
	// an empty database. An upgrade asks the database whether it is behind
	// the new package's schema before going ahead; this one has no schema
	// yet, so there is nothing to snapshot.
	host := func() string {
		GinkgoHelper()
		fedora, _ := fedoraBesideDatabase(dir, base, "sleep", "infinity")
		return fedora
	}

	// sh runs a script in the container and returns its stdout, exactly.
	sh := func(container, script string) string {
		GinkgoHelper()
		session := sessionIn(dir, 2*time.Minute, "podman", "exec", container, "sh", "-c", script)
		Expect(session).To(gexec.Exit(0), script)
		return string(session.Out.Contents())
	}

	// dnf installs or upgrades to a package from the host's disk.
	dnf := func(container, verb, pkg string) {
		GinkgoHelper()
		runIn(dir, time.Minute, "podman", "cp", pkg, container+":/tmp/")
		runIn(dir, 2*time.Minute, "podman", "exec", container,
			"dnf", "-y", "-q", "--disablerepo=*", verb, "/tmp/"+filepath.Base(pkg))
	}

	edit := func(container string) {
		GinkgoHelper()
		sh(container, `printf '%s\n' 'DATABASE_URL=`+podDatabaseURL+`' >>/etc/skift/env`)
	}

	settings := func(container string) []string {
		GinkgoHelper()
		return strings.Fields(sh(container, "ls -A /etc/skift"))
	}

	// templateDigest is the digest a package's header records for the
	// commented template.
	templateDigest := func(path string) string {
		GinkgoHelper()
		pkg, err := rpm.Open(path)
		Expect(err).NotTo(HaveOccurred())
		for _, f := range pkg.Files() {
			if f.Name() == "/usr/share/doc/skift/env.example" {
				return f.Digest()
			}
		}
		Fail(path + " carries no /usr/share/doc/skift/env.example")
		return ""
	}

	It("A first install creates the settings file", func() {
		By("given skift has never been installed")
		fedora := host()
		Expect(sh(fedora, "test -e /etc/skift/env || echo absent")).To(Equal("absent\n"))

		By("when the package is installed")
		dnf(fedora, "install", next)

		By("then /etc/skift/env exists, mode 0640, group skift")
		Expect(sh(fedora, "stat -c '%F %a %U %G' /etc/skift/env")).To(Equal("regular file 640 root skift\n"))
		By("and it points at the template")
		Expect(sh(fedora, "cat /etc/skift/env")).To(ContainSubstring("/usr/share/doc/skift/env.example"))
		Expect(sh(fedora, "test -s /usr/share/doc/skift/env.example && echo present")).To(Equal("present\n"))
	})

	It("An upgrade leaves the operator's settings alone", func() {
		By("given skift is installed and /etc/skift/env sets DATABASE_URL")
		fedora := host()
		dnf(fedora, "install", next)
		edit(fedora)
		before := sh(fedora, "cat /etc/skift/env")
		Expect(before).To(ContainSubstring("DATABASE_URL="))

		By("and the next package's environment template differs")
		Expect(templateDigest(later)).NotTo(Equal(templateDigest(next)))

		By("when the next package is installed over it")
		dnf(fedora, "upgrade", later)
		Expect(sh(fedora, "rpm -q skift")).To(HavePrefix("skift-9.9.9-1."))

		By("then /etc/skift/env is unchanged")
		Expect(sh(fedora, "cat /etc/skift/env")).To(Equal(before))
		Expect(sh(fedora, "stat -c '%a %U %G' /etc/skift/env")).To(Equal("640 root skift\n"))
		By("and no /etc/skift/env.rpmnew exists")
		Expect(settings(fedora)).NotTo(ContainElement("env.rpmnew"))
	})

	It("An upgrade from a release that shipped the template as the settings file keeps the operator's edits", func() {
		By("given " + configRelease + " is installed, with /etc/skift/env as its config file, and /etc/skift/env sets DATABASE_URL")
		fedora := host()
		dnf(fedora, "install", released)
		Expect(strings.Fields(sh(fedora, "rpm -q --configfiles skift"))).To(ContainElement("/etc/skift/env"))
		edit(fedora)
		before := sh(fedora, "cat /etc/skift/env")

		By("when this tree's package is installed over it")
		dnf(fedora, "upgrade", later)

		By("then /etc/skift/env is unchanged")
		Expect(sh(fedora, "cat /etc/skift/env")).To(Equal(before))
		Expect(sh(fedora, "stat -c '%a %U %G' /etc/skift/env")).To(Equal("640 root skift\n"))
		By("and neither /etc/skift/env.rpmnew nor /etc/skift/env.rpmsave exists")
		Expect(settings(fedora)).NotTo(ContainElement("env.rpmnew"))
		Expect(settings(fedora)).NotTo(ContainElement("env.rpmsave"))
	})

	It("An edited settings file survives removing the package", func() {
		By("given skift is installed and /etc/skift/env sets DATABASE_URL")
		fedora := host()
		dnf(fedora, "install", next)
		edit(fedora)
		before := sh(fedora, "cat /etc/skift/env")

		By("when dnf removes skift")
		sh(fedora, "dnf -y -q --disablerepo='*' remove skift")
		Expect(sh(fedora, "rpm -q skift || :")).To(ContainSubstring("not installed"))

		By("then /etc/skift/env is unchanged")
		Expect(sh(fedora, "cat /etc/skift/env")).To(Equal(before))
		Expect(sh(fedora, "stat -c '%a %U %G' /etc/skift/env")).To(Equal("640 root skift\n"))
		By("and no package owns it")
		Expect(sh(fedora, "rpm -qf /etc/skift/env || :")).To(ContainSubstring("not owned by any package"))

		By("and installing skift again leaves it as it was")
		dnf(fedora, "install", next)
		Expect(sh(fedora, "cat /etc/skift/env")).To(Equal(before))
		Expect(settings(fedora)).To(ConsistOf("env"))
	})

	It("Settings a release saved as env.rpmsave on removal are put back by the next install", func() {
		By("given " + configRelease + " was installed, /etc/skift/env set DATABASE_URL, and dnf removed skift")
		fedora := host()
		dnf(fedora, "install", released)
		edit(fedora)
		before := sh(fedora, "cat /etc/skift/env")
		sh(fedora, "dnf -y -q --disablerepo='*' remove skift")
		Expect(settings(fedora)).To(ConsistOf("env.rpmsave"))

		By("when this tree's package is installed")
		dnf(fedora, "install", later)

		By("then /etc/skift/env holds the saved settings, and no env.rpmsave remains")
		Expect(sh(fedora, "cat /etc/skift/env")).To(Equal(before))
		Expect(settings(fedora)).To(ConsistOf("env"))
	})

	It("rpm -V reports nothing for an edited settings file", func() {
		By("given skift is installed and /etc/skift/env has been edited")
		fedora := host()
		dnf(fedora, "install", next)
		edit(fedora)

		By("when rpm verifies the package")
		session := sessionIn(dir, time.Minute, "podman", "exec", fedora, "rpm", "-V", "skift")

		By("then it says nothing of /etc/skift/env")
		Expect(string(session.Out.Contents())).NotTo(ContainSubstring("/etc/skift/env"))
		Expect(session).To(gexec.Exit(0))
	})
})
