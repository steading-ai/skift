package acceptance_test

import (
	"context"
	"fmt"
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

// ADR-018 makes a downgrade a matter of installing the older release: it
// serves any schema whose compatibility floor it carries, even one a newer
// release migrated, and refuses any other. Here the newer release is a
// test-only migration set applied past this build's newest migration.

// appliedVersions reads every migration version goose recorded, so a
// scenario can tell that serve applied none.
func appliedVersions(ctx context.Context, url string) []int64 {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	rows, err := conn.Query(ctx, `SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id`)
	Expect(err).NotTo(HaveOccurred())
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	Expect(err).NotTo(HaveOccurred())
	return versions
}

var _ = Describe("Serving a schema a newer release migrated", Label(testsupport.DBLabel), func() {
	// start runs skift serve against url, as the unit starts it.
	start := func(url, socket string) *gexec.Session {
		GinkgoHelper()
		serve, err := gexec.Start(exec.Command(binary, "serve", "--socket", socket, "--database-url", url, "--shutdown-timeout", "5s"), GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			serve.Terminate().Wait(10 * time.Second)
			serve.Kill()
		})
		return serve
	}

	It("Scenario: An older release serves a newer schema within the floor", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)
		dir := GinkgoT().TempDir()
		Expect(operate(url, "admin", "migrate").ExitCode()).To(BeZero())

		By("given a database migrated past this binary's latest migration")
		latest := testsupport.MigratePast(ctx, url, testsupport.AddsColumn)
		applied := appliedVersions(ctx, url)
		Expect(applied[len(applied)-1]).To(Equal(latest + 1))

		By("and the floor is at or below this binary's latest migration")
		var floor int64
		conn, err := pgx.Connect(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		Expect(conn.QueryRow(ctx, `SELECT version FROM schema_floor`).Scan(&floor)).To(Succeed())
		Expect(conn.Close(ctx)).To(Succeed())
		Expect(floor).To(BeNumerically("<=", latest))

		By("and the caller bootstrapped by this binary's admin command")
		session := operate(url, "admin", "bootstrap", strconv.Itoa(os.Getuid()), "acme")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())

		By("when skift serve starts")
		socket := filepath.Join(dir, "skift.sock")
		serve := start(url, socket)

		By("then it reports ready")
		Eventually(serve.Err, 30*time.Second).Should(gbytes.Say("database reachable; ready"))

		By("and it writes and reads the newer schema, whose every table has a column it does not know")
		me := face{name: "host", flags: []string{"--socket", socket}, env: cleanEnv(dir), unix: true}
		Expect(me.createProject("Platform").GetSlug()).To(Equal(platform))
		me.patch(request(platform,
			createOpWith(&skiftv1.CreateNodeOp{Alias: "first", Title: "Written by the older release"}),
			createOpWith(&skiftv1.CreateNodeOp{Alias: "second", Title: "Waits on the first", BlockedBy: []string{"first"}}),
		))
		ready := me.ready()
		Expect(ready).To(HaveLen(1))
		Expect(ready[0].GetTitle()).To(Equal("Written by the older release"))
		claimed := me.claim()
		Expect(claimed.GetNode().GetNumber()).To(Equal(ready[0].GetNumber()))
		Expect(me.explain(ref(claimed.GetNode())).GetReasons()).To(ContainElement(HaveField("GetLeased()", Not(BeNil()))))
		Expect(me.explain("#2").GetReasons()).To(ContainElement(HaveField("GetBlocked()", Not(BeNil()))))
		listed := &skiftv1.ListNodesResponse{}
		me.call(listed, "node", "list", "--project", platform)
		Expect(listed.GetNodes()).To(HaveLen(2))

		By("and it applies no migration")
		Expect(string(serve.Err.Contents())).NotTo(ContainSubstring("migration applied"))
		Expect(appliedVersions(ctx, url)).To(Equal(applied))
	})

	It("Scenario: A release refuses a schema above its floor", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)
		Expect(operate(url, "admin", "migrate").ExitCode()).To(BeZero())

		By("given a database whose floor is above this binary's latest migration")
		latest := testsupport.MigratePast(ctx, url, testsupport.AddsColumn, testsupport.RaisesFloor)
		floor := latest + 2
		applied := appliedVersions(ctx, url)

		By("when skift serve starts")
		socket := filepath.Join(GinkgoT().TempDir(), "skift.sock")
		serve := start(url, socket)

		By("then it exits non-zero")
		Eventually(serve, 30*time.Second).Should(gexec.Exit())
		Expect(serve.ExitCode()).NotTo(BeZero())

		By("and the error names the floor and this binary's latest migration")
		Expect(string(serve.Err.Contents())).To(SatisfyAll(
			ContainSubstring(fmt.Sprintf("needs migration %d, and this build's newest is %d", floor, latest)),
			ContainSubstring(fmt.Sprintf("install skift that carries migration %d, or restore a snapshot", floor)),
		))
		Expect(appliedVersions(ctx, url)).To(Equal(applied))
	})
})
