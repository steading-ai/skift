package skift_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/steading-ai/skift/internal/admin"
)

// snapshotlessRelease is the last release without `skift admin snapshot`.
const snapshotlessRelease = "v0.4.0"

// newestMigration is the greatest version among goose's NNNNN_name.sql
// files in a tree, which is the schema a binary built from it migrates to.
func newestMigration(dir string) int64 {
	GinkgoHelper()
	files, err := filepath.Glob(filepath.Join(dir, "internal/store/migrations/*_*.sql"))
	Expect(err).NotTo(HaveOccurred())
	var newest int64
	for _, file := range files {
		prefix, _, _ := strings.Cut(filepath.Base(file), "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		Expect(err).NotTo(HaveOccurred(), file)
		newest = max(newest, v)
	}
	Expect(newest).To(BeNumerically(">", 0))
	return newest
}

// The rpm's pre-install snapshot, upgrading between packages built here on
// Fedora booted with systemd, beside a PostgreSQL of its own. Needs podman,
// like the database specs, so it carries their "db" label and stays out of
// `make test-unit`. Serial, as the other rpm specs are: it compiles skift
// and runs containers.
var _ = Describe("Upgrading the rpm", Label("db", "slow"), Serial, Ordered, func() {
	var (
		dir, base string
		schema    int64
		// installed has the snapshot command; same migrates nothing
		// further; migrating carries one migration more; snapshotless is
		// the last release without the command.
		installed, same, migrating, snapshotless string
	)

	BeforeAll(func() {
		dir = checkout()
		dist := filepath.Join(dir, "dist")
		schema = newestMigration(dir)
		runIn(dir, 10*time.Minute, "make", "package", "VERSION=9.9.7", "PACKAGE_ARCHES="+runtime.GOARCH)
		installed = rpmIn(dist, "9.9.7")
		runIn(dir, 10*time.Minute, "make", "package", "VERSION=9.9.8", "PACKAGE_ARCHES="+runtime.GOARCH)
		same = rpmIn(dist, "9.9.8")

		// The tree with one migration more, as the next release that
		// changes the schema would be.
		migration := fmt.Sprintf("internal/store/migrations/%05d_spec_upgrade.sql", schema+1)
		Expect(os.WriteFile(filepath.Join(dir, migration), []byte(
			"-- +goose Up\nCREATE TABLE spec_upgrade (id integer PRIMARY KEY);\n\n-- +goose Down\nDROP TABLE spec_upgrade;\n"), 0o644)).To(Succeed())
		runIn(dir, 10*time.Minute, "make", "package", "VERSION=9.9.9", "PACKAGE_ARCHES="+runtime.GOARCH)
		migrating = rpmIn(dist, "9.9.9")

		// The release as its own tree built it, binary and all: what its
		// binary lacks is the point.
		old := GinkgoT().TempDir()
		archive := filepath.Join(GinkgoT().TempDir(), "release.tar")
		runIn(".", time.Minute, "git", "archive", "--format=tar", "-o", archive, snapshotlessRelease)
		runIn(old, time.Minute, "tar", "-xf", archive)
		trustConfig(filepath.Join(old, "mise.toml"))
		runIn(old, 10*time.Minute, "make", "package", "VERSION=0.4.0", "PACKAGE_ARCHES="+runtime.GOARCH)
		snapshotless = rpmIn(filepath.Join(old, "dist"), "0.4.0")

		base = fedoraBase(dir)
	})

	// sh runs a script in a container and returns its stdout, exactly.
	sh := func(container, script string) string {
		GinkgoHelper()
		session := sessionIn(dir, 2*time.Minute, "podman", "exec", container, "sh", "-c", script)
		Expect(session).To(gexec.Exit(0), script)
		return string(session.Out.Contents())
	}

	// host starts Fedora booted with systemd that has never had skift
	// installed, beside an empty database. It returns both containers'
	// names.
	host := func() (fedora, db string) {
		GinkgoHelper()
		fedora, db = fedoraBesideDatabase(dir, base, "/usr/sbin/init")
		Eventually(func() string {
			return string(sessionIn(dir, time.Minute, "podman", "exec", fedora, "systemctl", "is-system-running").Out.Contents())
		}).WithTimeout(time.Minute).WithPolling(time.Second).Should(Or(Equal("running\n"), Equal("degraded\n")))
		return fedora, db
	}

	// dnf installs, upgrades or removes a package from the host's disk
	// with every repository off, and returns how that went.
	dnf := func(container, verb, pkg string) *gexec.Session {
		GinkgoHelper()
		runIn(dir, time.Minute, "podman", "cp", pkg, container+":/tmp/")
		return sessionIn(dir, 2*time.Minute, "podman", "exec", container,
			"dnf", "-y", "--disablerepo=*", verb, "/tmp/"+filepath.Base(pkg))
	}

	// said is everything a dnf run printed.
	said := func(session *gexec.Session) string {
		return string(session.Out.Contents()) + string(session.Err.Contents())
	}

	// configure points /etc/skift/env at the database beside it, quoted,
	// as an operator may write it.
	configure := func(fedora string) {
		GinkgoHelper()
		sh(fedora, `printf '%s\n' 'DATABASE_URL="`+podDatabaseURL+`"' >>/etc/skift/env`)
	}

	// serve starts skift.service, which is active once it has migrated
	// the database.
	serve := func(fedora string) {
		GinkgoHelper()
		sh(fedora, "systemctl enable --now skift.service")
		Expect(sh(fedora, "systemctl is-active skift.service")).To(Equal("active\n"))
	}

	schemaOf := func(db string) int64 {
		GinkgoHelper()
		out := runIn(dir, time.Minute, "podman", "exec", db, "psql", "--username", "skift", "--dbname", "skift",
			"--tuples-only", "--no-align", "--command", "SELECT max(version_id) FROM goose_db_version")
		v, err := strconv.ParseInt(out, 10, 64)
		Expect(err).NotTo(HaveOccurred())
		return v
	}

	// backups lists the snapshot directory, hidden files included.
	backups := func(fedora string) []string {
		GinkgoHelper()
		return strings.Fields(sh(fedora, "ls -A /var/lib/skift/backups 2>/dev/null || :"))
	}

	It("An upgrade that migrates is snapshotted first", func() {
		By("given skift is installed and serving a database")
		fedora, db := host()
		Expect(dnf(fedora, "install", installed)).To(gexec.Exit(0))
		configure(fedora)
		serve(fedora)
		Expect(schemaOf(db)).To(Equal(schema))

		By("when a package carrying a new migration is installed")
		Expect(dnf(fedora, "upgrade", migrating)).To(gexec.Exit(0))

		By("then a snapshot at the old schema version exists")
		files := backups(fedora)
		Expect(files).To(ConsistOf(MatchRegexp(`^skift-snapshot-\S+-schema%d\.zip$`, schema)))
		path := "/var/lib/skift/backups/" + files[0]
		Expect(sh(fedora, "stat -c '%U %G %a' "+path)).To(Equal("skift skift 600\n"), "the service can prune it")
		local := GinkgoT().TempDir()
		runIn(dir, time.Minute, "podman", "cp", fedora+":"+path, local)
		manifest, err := admin.ReadSnapshotManifest(filepath.Join(local, files[0]))
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.SchemaVersion).To(Equal(schema))
		Expect(manifest.SkiftVersion).To(Equal("9.9.7"), "the installed binary took it, before the new one ran")

		By("and the new release serves the migrated database")
		Expect(sh(fedora, "systemctl is-active skift.service")).To(Equal("active\n"))
		Expect(sh(fedora, "journalctl --no-pager --output cat --unit skift.service | grep 'skift serve starting' | tail -n 1")).
			To(ContainSubstring("version=9.9.9"), "the service was restarted onto the binary now installed")
		Expect(sh(fedora, "skift version")).To(Equal("skift version 9.9.9\n"))
		Expect(schemaOf(db)).To(Equal(schema + 1))
	})

	It("A failed snapshot means no upgrade", func() {
		By("given skift is installed and the database cannot be reached")
		fedora, db := host()
		Expect(dnf(fedora, "install", installed)).To(gexec.Exit(0))
		configure(fedora)
		serve(fedora)
		runIn(dir, time.Minute, "podman", "stop", "--time", "2", db)
		Expect(sh(fedora, "systemctl is-active skift.service")).To(Equal("active\n"))

		By("when a newer package is installed")
		session := dnf(fedora, "upgrade", migrating)

		By("then the install fails")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(said(session)).To(ContainSubstring("snapshot before upgrading to 9.9.9 failed"))
		Expect(said(session)).To(ContainSubstring("skift version 9.9.7 is still running"))
		Expect(sh(fedora, "journalctl --no-pager --output cat --identifier skift-upgrade")).
			To(ContainSubstring("skift version 9.9.7 is still running"), "dnf5-automatic's report leaves scriptlet output out")

		By("and the installed version is unchanged")
		Expect(sh(fedora, "rpm -q skift")).To(HavePrefix("skift-9.9.7-1."))
		Expect(sh(fedora, "/usr/bin/skift version")).To(Equal("skift version 9.9.7\n"))
		Expect(sh(fedora, "systemctl is-active skift.service")).To(Equal("active\n"))
		Expect(backups(fedora)).To(BeEmpty())
	})

	It("Nothing to migrate means no snapshot", func() {
		By("given skift is installed at the newest schema")
		fedora, db := host()
		Expect(dnf(fedora, "install", installed)).To(gexec.Exit(0))
		configure(fedora)
		serve(fedora)
		Expect(schemaOf(db)).To(Equal(schema))

		By("when a package with no new migration is installed")
		session := dnf(fedora, "upgrade", same)
		Expect(session).To(gexec.Exit(0))
		Expect(sh(fedora, "rpm -q skift")).To(HavePrefix("skift-9.9.8-1."))

		By("then no snapshot is written")
		Expect(backups(fedora)).To(BeEmpty())
		Expect(said(session)).To(ContainSubstring(fmt.Sprintf("no snapshot taken: the database is at schema %d", schema)))
	})

	It("A first install takes no snapshot, even of a configured database it would migrate", func() {
		By("given a database a removed skift served, and the settings it left")
		fedora, db := host()
		Expect(dnf(fedora, "install", installed)).To(gexec.Exit(0))
		configure(fedora)
		serve(fedora)
		sh(fedora, "dnf -y -q --disablerepo='*' remove skift")
		Expect(schemaOf(db)).To(Equal(schema))

		By("when a package carrying a new migration is installed")
		session := dnf(fedora, "install", migrating)
		Expect(session).To(gexec.Exit(0))

		By("then no snapshot is written")
		Expect(backups(fedora)).To(BeEmpty())
		Expect(said(session)).NotTo(ContainSubstring("snapshot"))
	})

	It("An upgrade from a release without admin snapshot goes ahead and says no snapshot was taken", func() {
		By("given " + snapshotlessRelease + " is installed and /etc/skift/env names a database")
		fedora, _ := host()
		Expect(dnf(fedora, "install", snapshotless)).To(gexec.Exit(0))
		configure(fedora)

		By("when a package carrying a new migration is installed")
		session := dnf(fedora, "upgrade", migrating)

		By("then it is installed, and says why there is no snapshot")
		Expect(session).To(gexec.Exit(0))
		Expect(sh(fedora, "rpm -q skift")).To(HavePrefix("skift-9.9.9-1."))
		Expect(said(session)).To(ContainSubstring("no snapshot taken before upgrading to 9.9.9: the installed skift has no admin snapshot command"))
		Expect(backups(fedora)).To(BeEmpty())
	})

	// The release owned /etc/skift/env, so rpm moves it aside as the
	// release is erased, and the release's postremove restarts the service
	// before posttrans.sh puts it back.
	It("A service running a release that owned the settings file is serving the new release when the upgrade ends", func() {
		By("given " + snapshotlessRelease + " is installed and serving the database /etc/skift/env names")
		fedora, db := host()
		Expect(dnf(fedora, "install", snapshotless)).To(gexec.Exit(0))
		configure(fedora)
		serve(fedora)

		By("when a package carrying a new migration is installed")
		Expect(dnf(fedora, "upgrade", migrating)).To(gexec.Exit(0))

		By("then the service is active as dnf returns, not waiting to restart, on the new release with its settings")
		Expect(sh(fedora, "systemctl show --property ActiveState --value skift.service")).To(Equal("active\n"))
		Expect(sh(fedora, "journalctl --no-pager --output cat --unit skift.service | grep 'skift serve starting' | tail -n 1")).
			To(ContainSubstring("version=9.9.9"))
		Expect(schemaOf(db)).To(Equal(schema + 1))
		Expect(strings.Fields(sh(fedora, "ls -A /etc/skift"))).To(ConsistOf("env"))
	})

	It("A stopped service stays stopped when the settings file is put back", func() {
		By("given " + snapshotlessRelease + " is installed, configured and stopped")
		fedora, _ := host()
		Expect(dnf(fedora, "install", snapshotless)).To(gexec.Exit(0))
		configure(fedora)

		By("when a newer package is installed")
		Expect(dnf(fedora, "upgrade", migrating)).To(gexec.Exit(0))

		By("then the settings are back and the service is not started")
		Expect(strings.Fields(sh(fedora, "ls -A /etc/skift"))).To(ConsistOf("env"))
		Expect(sh(fedora, "systemctl show --property ActiveState --value skift.service")).To(Equal("inactive\n"))
	})

	It("An upgrade on a host whose settings name no database goes ahead and says no snapshot was taken", func() {
		By("given skift is installed and /etc/skift/env sets no DATABASE_URL")
		fedora, _ := host()
		Expect(dnf(fedora, "install", installed)).To(gexec.Exit(0))

		By("when a package carrying a new migration is installed")
		session := dnf(fedora, "upgrade", migrating)

		By("then it is installed, and says why there is no snapshot")
		Expect(session).To(gexec.Exit(0))
		Expect(sh(fedora, "rpm -q skift")).To(HavePrefix("skift-9.9.9-1."))
		Expect(said(session)).To(ContainSubstring("no snapshot taken before upgrading to 9.9.9: /etc/skift/env sets no DATABASE_URL"))
		Expect(backups(fedora)).To(BeEmpty())
	})
})
