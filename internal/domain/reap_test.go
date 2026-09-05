package domain_test

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// expireEvents counts the expiry events on one node.
func (g *graph) expireEvents(ctx context.Context, ref string) int {
	GinkgoHelper()
	n := 0
	for _, e := range g.nodeEvents(ctx, ref) {
		if e.Op == domain.OpExpireLease {
			n++
		}
	}
	return n
}

// reap runs the reaper across the deployment and returns what it did in
// the spec's organization.
func (g *graph) reap(ctx context.Context) domain.Reaped {
	GinkgoHelper()
	reaped, err := domain.Reap(ctx, g.s, pol, 0)
	Expect(err).NotTo(HaveOccurred())
	for _, r := range reaped {
		if r.OrgID == g.org {
			return r
		}
	}
	Fail("the reaper did not visit organization " + g.org)
	return domain.Reaped{}
}

var _ = Describe("Reaper", Label(testsupport.DBLabel), func() {
	var (
		s     *store.Store
		g     *graph
		actor domain.Actor
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		actor = g.principal(ctx)
	})

	Describe("Scenarios", func() {
		It("An abandoned lease is reclaimed", func(ctx SpecContext) {
			By("given a lease whose expiry passed 10 minutes ago")
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			claimed := g.mustClaim(ctx, actor, claimReq("one", 0))
			Expect(g.ready(ctx, domain.ReadyQuery{})).To(BeEmpty())
			g.age(ctx, claimed.Lease.ID, 25*time.Minute, -10*time.Minute)

			By("when the reaper runs")
			reaped := g.reap(ctx)

			By(`then the lease is released with reason "expired"`)
			Expect(reaped.Expired).To(HaveLen(1))
			Expect(reaped.Expired[0].ID).To(Equal(claimed.Lease.ID))
			lease := g.getLease(ctx, claimed.Lease.ID)
			Expect(lease.ReleasedAt).NotTo(BeNil())
			Expect(*lease.ReleaseReason).To(Equal(domain.ReleaseExpired))
			events := g.nodeEvents(ctx, "#1")
			Expect(ops(events)).To(Equal([]string{domain.OpClaimNode, domain.OpExpireLease}))
			Expect(*events[1].Reason).To(Equal(domain.ReleaseExpired))
			Expect(events[1].PrincipalKind).To(Equal("service"))
			Expect(events[1].IdentityScheme).To(Equal(domain.ReaperIdentityScheme))
			Expect(events[1].RequestID).To(BeNil())

			By("and the node is ready again")
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))
		})

		It("Two reapers do not double-release", func(ctx SpecContext) {
			By("given an expired lease")
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			claimed := g.mustClaim(ctx, actor, claimReq("one", 0))
			g.age(ctx, claimed.Lease.ID, time.Hour, -10*time.Minute)

			By("when two reapers run concurrently against the same expired lease")
			const reapers = 2
			results := make([]domain.Reaped, reapers)
			errs := make([]error, reapers)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range reapers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer GinkgoRecover()
					<-start
					reaped, err := domain.Reap(ctx, s, pol, 0)
					errs[i] = err
					for _, r := range reaped {
						if r.OrgID == g.org {
							results[i] = r
						}
					}
				}()
			}
			close(start)
			wg.Wait()

			By("then exactly one release event is written")
			for i := range reapers {
				Expect(errs[i]).NotTo(HaveOccurred())
			}
			Expect(len(results[0].Expired) + len(results[1].Expired)).To(Equal(1))
			Expect(g.expireEvents(ctx, "#1")).To(Equal(1))
			Expect(*g.getLease(ctx, claimed.Lease.ID).ReleaseReason).To(Equal(domain.ReleaseExpired))
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))
		})
	})

	It("is idempotent: a second run finds nothing and writes nothing", func(ctx SpecContext) {
		g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
		claimed := g.mustClaim(ctx, actor, claimReq("one", 0))
		g.age(ctx, claimed.Lease.ID, time.Hour, -time.Minute)

		Expect(g.reap(ctx).Expired).To(HaveLen(1))
		Expect(g.reap(ctx).Expired).To(BeEmpty())
		Expect(g.expireEvents(ctx, "#1")).To(Equal(1))
	})

	It("leaves live leases alone and pages through a backlog of expired ones", func(ctx SpecContext) {
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			for i := 1; i <= 5; i++ {
				g.node(ctx, tx, int64(i), nil)
			}
		})
		// Every claim comes before any lease is aged: a claim expires a
		// stale lease on the node it takes, and these must be left for
		// the reaper.
		var stale []string
		for i := 1; i <= 4; i++ {
			stale = append(stale, g.mustClaim(ctx, actor, claimReq(string(rune('a'+i)), 0)).Lease.ID)
		}
		live := g.mustClaim(ctx, actor, claimReq("live", 0))
		for i, id := range stale {
			g.age(ctx, id, time.Hour, -time.Duration(i+1)*time.Minute)
		}

		reaped, err := domain.Reap(ctx, s, pol, 2)
		Expect(err).NotTo(HaveOccurred())
		var ids []string
		for _, r := range reaped {
			if r.OrgID == g.org {
				for _, l := range r.Expired {
					ids = append(ids, l.ID)
				}
			}
		}
		Expect(ids).To(ConsistOf(stale))
		Expect(g.getLease(ctx, live.Lease.ID).ReleasedAt).To(BeNil())
		Expect(g.liveLeases(ctx)).To(Equal(1))
	})

	It("acts as one service principal per organization, created on first use and reused after", func(ctx SpecContext) {
		g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
		first := g.mustClaim(ctx, actor, claimReq("one", 0))
		g.age(ctx, first.Lease.ID, time.Hour, -time.Minute)
		g.reap(ctx)
		second := g.mustClaim(ctx, actor, claimReq("two", 0))
		g.age(ctx, second.Lease.ID, time.Hour, -time.Minute)
		g.reap(ctx)

		events := g.nodeEvents(ctx, "#1")
		Expect(ops(events)).To(Equal([]string{domain.OpClaimNode, domain.OpExpireLease, domain.OpClaimNode, domain.OpExpireLease}))
		Expect(events[1].PrincipalID).To(Equal(events[3].PrincipalID))
		Expect(events[1].PrincipalID).NotTo(Equal(actor.PrincipalID))

		var n int
		Expect(s.Pool().QueryRow(ctx, `SELECT count(*) FROM principals WHERE org_id = $1 AND kind = 'service'`, g.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})
})
