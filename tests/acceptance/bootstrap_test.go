package acceptance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/testsupport"
)

// A machine with the package installed has a binary, a unit, and an empty
// database. What the package cannot know is the uid of the caller, so the
// path from there to a working skift is three commands of the binary's
// own: migrate, bootstrap for that uid, and start. Nothing here touches
// the database except to count what bootstrap left.

// principalCount reads how many principals the database holds.
func principalCount(ctx context.Context, url string) int {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	var n int
	Expect(conn.QueryRow(ctx, `SELECT count(*) FROM principals`).Scan(&n)).To(Succeed())
	return n
}

// admin runs one of the binary's database-facing commands to completion.
func admin(url string, args ...string) *gexec.Session {
	GinkgoHelper()
	session, err := gexec.Start(exec.Command(binary, append(args, "--database-url", url)...), GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session, 60*time.Second).Should(gexec.Exit(), "skift %v did not exit", args)
	return session
}

var _ = Describe("Bootstrapping a deployment", Label(testsupport.DBLabel), func() {
	It("Scenario: A clean machine becomes a working skift", func(ctx SpecContext) {
		By("given the skift binary and a reachable empty database")
		url := pg.EmptyDatabase(ctx)
		dir := GinkgoT().TempDir()
		uid := strconv.Itoa(os.Getuid())

		By("when skift migrate runs")
		session := admin(url, "migrate")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())

		By("and skift admin bootstrap runs for the caller's user id")
		session = admin(url, "admin", "bootstrap", uid, "acme")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())

		By("and the service is started, as the unit starts it")
		socket := filepath.Join(dir, "skift.sock")
		serve, err := gexec.Start(exec.Command(binary, "serve", "--socket", socket, "--database-url", url, "--shutdown-timeout", "5s"), GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			serve.Terminate().Wait(10 * time.Second)
			serve.Kill()
		})
		Eventually(serve.Err, 30*time.Second).Should(gbytes.Say("database reachable; ready"))

		By("then skift ready answers the bootstrapped user with an empty ready set and no error")
		me := face{name: "host", flags: []string{"--socket", socket}, env: cleanEnv(dir), unix: true}
		project := me.createProject("Platform")
		Expect(project.GetSlug()).To(Equal(platform))
		res := &skiftv1.ReadyResponse{}
		me.call(res, "ready", "--project", platform)
		Expect(res.GetNodes()).To(BeEmpty())
	})

	It("Scenario: Under --no-migrate the service refuses a database that is behind", func(ctx SpecContext) {
		By("given an empty database and a service told to apply nothing")
		url := pg.EmptyDatabase(ctx)
		socket := filepath.Join(GinkgoT().TempDir(), "skift.sock")
		start := func() *gexec.Session {
			GinkgoHelper()
			session, err := gexec.Start(exec.Command(binary, "serve", "--socket", socket, "--database-url", url, "--no-migrate", "--shutdown-timeout", "5s"), GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			return session
		}

		By("when skift serve starts")
		serve := start()

		By("then it exits, naming the versions and the command that fixes it")
		Eventually(serve, 30*time.Second).Should(gexec.Exit())
		Expect(serve.ExitCode()).NotTo(BeZero())
		Expect(string(serve.Err.Contents())).To(SatisfyAll(
			ContainSubstring("schema is at version 0"),
			ContainSubstring("run skift migrate"),
		))

		By("and once migrated, the same command serves")
		Expect(admin(url, "migrate").ExitCode()).To(BeZero())
		serve = start()
		DeferCleanup(func() {
			serve.Terminate().Wait(10 * time.Second)
			serve.Kill()
		})
		Eventually(serve.Err, 30*time.Second).Should(gbytes.Say("database reachable; ready"))
		Expect(string(serve.Err.Contents())).To(ContainSubstring("schema current"))
	})

	It("Scenario: Bootstrap is safe against an already-seeded database", func(ctx SpecContext) {
		By("given a database that has already been bootstrapped")
		url := pg.EmptyDatabase(ctx)
		Expect(admin(url, "migrate").ExitCode()).To(BeZero())
		Expect(admin(url, "admin", "bootstrap", "1000", "acme").ExitCode()).To(BeZero())
		before := principalCount(ctx, url)

		By("when skift admin bootstrap is run again with the same user id")
		session := admin(url, "admin", "bootstrap", "1000", "acme")

		By("then it exits zero and the principal count is unchanged")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		Expect(principalCount(ctx, url)).To(Equal(before))
	})
})
