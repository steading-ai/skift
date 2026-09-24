package acceptance_test

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/testsupport"
)

// reapSummary is what one run of skift reap prints.
type reapSummary struct {
	Expired       int `json:"expired"`
	Organizations []struct {
		OrgID   string   `json:"org_id"`
		Expired int      `json:"expired"`
		Leases  []string `json:"leases"`
	} `json:"organizations"`
}

// reap runs the expiry timer's command against the scenario's database
// and returns its summary.
func reap(url string) reapSummary {
	GinkgoHelper()
	session := admin(url, "reap")
	Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
	var summary reapSummary
	Expect(json.Unmarshal(session.Out.Contents(), &summary)).To(Succeed(), "stdout: %s", session.Out.Contents())
	return summary
}

var _ = Describe("Expiring leases", Label(testsupport.DBLabel), func() {
	It("Scenario: The expiry timer reclaims an abandoned lease", func(ctx SpecContext) {
		d := deploy(ctx)

		By("given a node claimed under a one-second lease that is never renewed")
		d.laptop.createProject("Platform")
		res := d.laptop.patch(request(platform, createOpWith(&skiftv1.CreateNodeOp{Alias: "a", Title: "Work"})))
		node := res.GetNodes()[0]
		claimed := d.host.claim("--ttl", "1s")
		Expect(claimed.GetNode().GetNumber()).To(Equal(node.GetNumber()))
		Expect(d.host.ready()).To(BeEmpty())

		By("when skift reap runs against the database once the TTL has passed, as the timer runs it")
		var summary reapSummary
		Eventually(func() int {
			summary = reap(d.databaseURL)
			return summary.Expired
		}, 15*time.Second, time.Second).Should(Equal(1))

		By("then the run names the lease it released, under its organization")
		Expect(summary.Organizations).To(HaveLen(1))
		Expect(summary.Organizations[0].OrgID).To(Equal(orgID))
		Expect(summary.Organizations[0].Expired).To(Equal(1))
		Expect(summary.Organizations[0].Leases).To(Equal([]string{claimed.GetLease().GetId()}))

		By("and the node is ready again")
		Expect(numbers(d.host.ready())).To(Equal([]int64{node.GetNumber()}))
		Expect(d.host.explain(ref(node)).GetReasons()).To(BeEmpty())

		By("and a second run finds nothing to expire")
		Expect(reap(d.databaseURL).Expired).To(BeZero())
	})
})
