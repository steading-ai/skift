package acceptance_test

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/testsupport"
)

// jobRun mirrors one run as skift admin jobs prints it.
type jobRun struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Error   string `json:"error"`
}

// jobStatus mirrors one job as skift admin jobs list prints it.
type jobStatus struct {
	Name    string  `json:"name"`
	Every   string  `json:"every"`
	LastRun *jobRun `json:"last_run"`
}

// Lease expiry is a background job serve runs itself, so it happens in
// every deployment shape, with no timer beside the service. See
// docs/adrs/016-background-jobs-and-admin.md.
var _ = Describe("Expiring leases", Label(testsupport.DBLabel), func() {
	// claimAbandoned claims the only node under a one-second lease that is
	// never renewed, waits for the lease's TTL to pass, and returns the node
	// and the lease's id.
	claimAbandoned := func(d *deployment) (*skiftv1.Node, string) {
		GinkgoHelper()
		d.laptop.createProject("Platform")
		res := d.laptop.patch(request(platform, createOpWith(&skiftv1.CreateNodeOp{Alias: "a", Title: "Work"})))
		node := res.GetNodes()[0]
		claimed := d.host.claim("--ttl", "1s")
		Expect(claimed.GetNode().GetNumber()).To(Equal(node.GetNumber()))
		time.Sleep(1500 * time.Millisecond)
		return node, claimed.GetLease().GetId()
	}

	// releaseReason is why the lease was released, read straight from the
	// database: empty while it is unreleased. A lease past its TTL already
	// leaves the ready set, so readiness alone cannot show the job ran.
	releaseReason := func(ctx context.Context, url, leaseID string) string {
		GinkgoHelper()
		conn, err := pgx.Connect(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close(ctx) //nolint:errcheck // single-statement session
		var reason *string
		Expect(conn.QueryRow(ctx, `SELECT release_reason FROM leases WHERE id = $1`, leaseID).Scan(&reason)).To(Succeed())
		if reason == nil {
			return ""
		}
		return *reason
	}

	// startRunDone waits until serve has made its first expire-leases run,
	// so the next scheduled one is a minute away.
	startRunDone := func(d *deployment) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			session := operate(d.databaseURL, "admin", "jobs", "list")
			g.Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
			var statuses []jobStatus
			g.Expect(json.Unmarshal(session.Out.Contents(), &statuses)).To(Succeed())
			g.Expect(statuses).To(ContainElement(SatisfyAll(
				HaveField("Name", "expire-leases"),
				HaveField("LastRun.Outcome", "completed"),
			)))
		}, 30*time.Second, time.Second).Should(Succeed())
	}

	It("Scenario: Stale leases expire in a container install", func(ctx SpecContext) {
		By("given skift serve running with no reaper timer")
		d := deploy(ctx)
		// serve runs expire-leases once as it starts. Waiting that run out
		// first means the expiry below is the scheduled run's, a minute on,
		// not the start-up run reaching the lease before the spec looks.
		startRunDone(d)

		By("and a lease whose TTL has passed")
		node, lease := claimAbandoned(d)
		Expect(releaseReason(ctx, d.databaseURL, lease)).To(BeEmpty())

		By("when a minute passes")
		By("then the lease is expired and its node is ready again")
		Eventually(func() string { return releaseReason(ctx, d.databaseURL, lease) }, 75*time.Second, time.Second).Should(Equal("expired"))
		Expect(numbers(d.host.ready())).To(Equal([]int64{node.GetNumber()}))
		Expect(d.host.explain(ref(node)).GetReasons()).To(BeEmpty())
	})

	It("expires a stale lease on demand with skift admin jobs run --wait, and prints the outcome", func(ctx SpecContext) {
		d := deploy(ctx)
		startRunDone(d)
		node, lease := claimAbandoned(d)

		By("given the lease's TTL has passed and the minute's run has not come round")
		Expect(releaseReason(ctx, d.databaseURL, lease)).To(BeEmpty())

		By("when skift admin jobs run expire-leases --wait runs")
		session := operate(d.databaseURL, "admin", "jobs", "run", "expire-leases", "--wait")

		By("then it exits zero and prints a completed run")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		var run jobRun
		Expect(json.Unmarshal(session.Out.Contents(), &run)).To(Succeed(), "stdout: %s", session.Out.Contents())
		Expect(run.Name).To(Equal("expire-leases"))
		Expect(run.Outcome).To(Equal("completed"))

		By("and the lease is expired, and the node is ready again")
		Expect(releaseReason(ctx, d.databaseURL, lease)).To(Equal("expired"))
		Expect(numbers(d.host.ready())).To(Equal([]int64{node.GetNumber()}))
	})

	It("lists expire-leases with its schedule and the run serve made at start", func(ctx SpecContext) {
		d := deploy(ctx)

		By("when skift admin jobs list runs")
		By("then it names expire-leases, every minute, with a completed run")
		// The start-up run is listed as soon as it is scheduled, and shows
		// as running until it finishes, so the list is read until it has.
		Eventually(func(g Gomega) {
			session := operate(d.databaseURL, "admin", "jobs", "list")
			g.Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
			var statuses []jobStatus
			g.Expect(json.Unmarshal(session.Out.Contents(), &statuses)).To(Succeed())
			g.Expect(statuses).To(ContainElement(SatisfyAll(
				HaveField("Name", "expire-leases"),
				HaveField("Every", "1m0s"),
				HaveField("LastRun.Outcome", "completed"),
			)))
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})
