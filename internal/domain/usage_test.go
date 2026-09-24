package domain_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// reportUsage is a report_usage operation naming a model and its input
// and output token counts.
func reportUsage(node, model string, in, out int64) domain.Op {
	return domain.Op{ReportUsage: &domain.ReportUsage{Node: node, Model: model, Tokens: domain.Tokens{InputTokens: in, OutputTokens: out}}}
}

// estimate is an estimate_node operation with a basis.
func estimate(node, model string, in, out int64, basis string) domain.Op {
	return domain.Op{EstimateNode: &domain.EstimateNode{Node: node, Model: model, Tokens: domain.Tokens{InputTokens: in, OutputTokens: out}, Basis: basis}}
}

// usage reads every usage record of a node in a transaction of its own.
func (g *graph) usage(ctx context.Context, ref string) []store.Usage {
	GinkgoHelper()
	var rows []store.Usage
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		rows, err = domain.ListUsage(ctx, tx, g.org, g.id(ref))
		return err
	})).To(Succeed())
	return rows
}

func (g *graph) currentEstimate(ctx context.Context, ref string) *store.Usage {
	GinkgoHelper()
	var u *store.Usage
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		u, err = domain.CurrentEstimate(ctx, tx, g.org, g.id(ref))
		return err
	})).To(Succeed())
	return u
}

func (g *graph) rollup(ctx context.Context, ref string) *domain.Rollup {
	GinkgoHelper()
	var r *domain.Rollup
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		r, err = domain.SubtreeRollup(ctx, tx, g.org, g.id(ref))
		return err
	})).To(Succeed())
	return r
}

// ref is the `#number` reference of a claimed node.
func ref(c *domain.Claimed) string {
	return fmt.Sprintf("#%d", c.Node.Number)
}

// totals is a rollup group as a spec expects it.
func totals(model string, records, in, out int64) domain.ModelTotals {
	return domain.ModelTotals{Model: &model, Records: records, Tokens: domain.Tokens{InputTokens: in, OutputTokens: out}}
}

var _ = Describe("Usage records and cost rollups", Label(testsupport.DBLabel), func() {
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

	// runAttempt claims the next ready node as a fresh agent, reports the
	// given usage against the attempt, and completes the node, which
	// releases the lease with it.
	runAttempt := func(ctx context.Context, model string, in, out int64) *domain.Claimed {
		GinkgoHelper()
		agent := g.agent(ctx)
		c := g.mustClaim(ctx, agent, claimReq("claim-"+model+"-"+fmt.Sprint(in), time.Minute))
		_, err := g.applyWithLease(ctx, agent, "work-"+c.Lease.ID, c.Lease.ID,
			reportUsage(ref(c), model, in, out), complete(ref(c), "done"))
		Expect(err).NotTo(HaveOccurred())
		Expect(g.getLease(ctx, c.Lease.ID).ReleaseReason).To(HaveValue(Equal(domain.ReleaseComplete)))
		return c
	}

	Describe("Scenarios", func() {
		It("Planning from actuals", func(ctx SpecContext) {
			By("given three sibling nodes completed with recorded usage")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				parent := g.node(ctx, tx, 1, nil)
				for n := int64(2); n <= 5; n++ {
					g.node(ctx, tx, n, &parent)
				}
			})
			done := []string{ref(runAttempt(ctx, "opus", 1000, 100)), ref(runAttempt(ctx, "opus", 3000, 300)), ref(runAttempt(ctx, "sonnet", 500, 50))}
			Expect(g.statuses(ctx, done...)).To(Equal([]string{"done", "done", "done"}))

			By("when an agent estimates the remaining work")
			agent := g.agent(ctx)
			c := g.mustClaim(ctx, agent, claimReq("claim-remaining", time.Minute))
			Expect(done).NotTo(ContainElement(ref(c)))
			Expect(g.getNode(ctx, ref(c)).ParentID).To(HaveValue(Equal(g.id("#1"))))
			r := g.rollup(ctx, "#1")
			var byModel []domain.ModelTotals
			var in, out int64
			for _, t := range r.Actual {
				byModel = append(byModel, t)
				in += t.InputTokens
				out += t.OutputTokens
			}
			mean := domain.Tokens{InputTokens: in / int64(len(byModel)+1), OutputTokens: out / int64(len(byModel)+1)}
			res, err := g.applyWithLease(ctx, agent, "estimate", c.Lease.ID,
				estimate(ref(c), "opus", mean.InputTokens, mean.OutputTokens, "mean of siblings "+strings.Join(done, ", ")))
			Expect(err).NotTo(HaveOccurred())

			By("then it can read actual usage grouped by model for the completed siblings")
			Expect(byModel).To(Equal([]domain.ModelTotals{
				totals("opus", 2, 4000, 400),
				totals("sonnet", 1, 500, 50),
			}))
			Expect(r.Estimate).To(BeEmpty())

			By("and the estimate is recorded with its author and basis")
			Expect(res.Usage).To(HaveLen(1))
			e := res.Usage[0]
			Expect(e.Kind).To(Equal(domain.UsageEstimate))
			Expect(e.Principal).To(Equal(agent.PrincipalID))
			Expect(e.Basis).To(HaveValue(HavePrefix("mean of siblings #")))
			Expect(e.LeaseID).To(BeNil())
			Expect(*g.currentEstimate(ctx, ref(c))).To(Equal(e))
			Expect(g.rollup(ctx, "#1").Estimate).To(Equal([]domain.ModelTotals{totals("opus", 1, 1500, 150)}))
			Expect(ops(res.Events)).To(Equal([]string{domain.OpEstimateNode}))
		})

		It("A retry is visible as a separate attempt", func(ctx SpecContext) {
			By("given a node claimed twice, each attempt reporting usage")
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 7, nil) })
			agentA, agentB := g.agent(ctx), g.agent(ctx)
			first := g.mustClaim(ctx, agentA, claimReq("a-claim", time.Minute))
			_, err := g.applyWithLease(ctx, agentA, "a-usage", first.Lease.ID, reportUsage("#7", "opus", 1000, 100))
			Expect(err).NotTo(HaveOccurred())

			g.age(ctx, first.Lease.ID, 2*time.Minute, -time.Minute)
			second := g.mustClaim(ctx, agentB, claimReq("b-claim", time.Minute))
			Expect(second.Node.ID).To(Equal(first.Node.ID))
			Expect(second.Lease.ID).NotTo(Equal(first.Lease.ID))
			_, err = g.applyWithLease(ctx, agentB, "b-usage", second.Lease.ID, reportUsage("#7", "opus", 2000, 200))
			Expect(err).NotTo(HaveOccurred())

			By("when I read its usage")
			rows := g.usage(ctx, "#7")

			By("then two rows exist with different lease ids")
			Expect(rows).To(HaveLen(2))
			Expect(rows[0].LeaseID).To(HaveValue(Equal(first.Lease.ID)))
			Expect(rows[1].LeaseID).To(HaveValue(Equal(second.Lease.ID)))
			Expect(rows[0].Principal).To(Equal(agentA.PrincipalID))
			Expect(rows[1].Principal).To(Equal(agentB.PrincipalID))
			for _, u := range rows {
				Expect(u.NodeID).To(Equal(first.Node.ID))
				Expect(u.Kind).To(Equal(domain.UsageActual))
			}
			Expect(g.rollup(ctx, "#7").Actual).To(Equal([]domain.ModelTotals{totals("opus", 2, 3000, 300)}))
		})
	})

	Describe("Reporting an actual", func() {
		BeforeEach(func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, nil)
			})
		})

		It("attributes the report to the lease the patch carries and renews it", func(ctx SpecContext) {
			c := g.mustClaim(ctx, actor, claimReq("claim", time.Minute))
			g.age(ctx, c.Lease.ID, 50*time.Second, 10*time.Second)

			r, err := g.applyWithLease(ctx, actor, "usage", c.Lease.ID,
				domain.Op{ReportUsage: &domain.ReportUsage{Node: ref(c), Model: "opus",
					Tokens: domain.Tokens{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40}}})
			Expect(err).NotTo(HaveOccurred())

			Expect(r.Usage).To(HaveLen(1))
			u := r.Usage[0]
			Expect(u.LeaseID).To(HaveValue(Equal(c.Lease.ID)))
			Expect(u.NodeID).To(Equal(c.Node.ID))
			Expect(u.Principal).To(Equal(actor.PrincipalID))
			Expect(u.Kind).To(Equal(domain.UsageActual))
			Expect(u.Model).To(HaveValue(Equal("opus")))
			Expect(u.Basis).To(BeNil())
			Expect([]int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens}).To(Equal([]int64{10, 20, 30, 40}))
			Expect(ops(r.Events)).To(Equal([]string{domain.OpReportUsage}))
			Expect(g.getLease(ctx, c.Lease.ID).ExpiresAt).To(BeTemporally("~", time.Now().Add(domain.DefaultLeaseTTL), 5*time.Second))
		})

		It("needs a lease", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "usage", reportUsage("#1", "opus", 1, 1))
			Expect(err).To(MatchError(domain.ErrUsageNeedsLease))
			Expect(g.usage(ctx, "#1")).To(BeEmpty())
			Expect(g.countEvents(ctx, "usage")).To(BeZero())
		})

		It("is attributed to the node the lease holds and no other", func(ctx SpecContext) {
			c := g.mustClaim(ctx, actor, claimReq("claim", time.Minute))
			other := "#2"
			if c.Node.Number == 2 {
				other = "#1"
			}
			_, err := g.applyWithLease(ctx, actor, "usage", c.Lease.ID, reportUsage(other, "opus", 1, 1))
			Expect(err).To(MatchError(domain.ErrLeaseMismatch))
			Expect(g.usage(ctx, other)).To(BeEmpty())
		})

		It("is rejected once the lease is no longer live", func(ctx SpecContext) {
			c := g.mustClaim(ctx, actor, claimReq("claim", time.Minute))
			g.age(ctx, c.Lease.ID, 2*time.Minute, -time.Minute)
			_, err := g.applyWithLease(ctx, actor, "usage", c.Lease.ID, reportUsage(ref(c), "opus", 1, 1))
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))
		})

		It("rejects counts that are negative or all zero", func(ctx SpecContext) {
			c := g.mustClaim(ctx, actor, claimReq("claim", time.Minute))
			_, err := g.applyWithLease(ctx, actor, "negative", c.Lease.ID, reportUsage(ref(c), "opus", -1, 1))
			Expect(err).To(MatchError(domain.ErrNegativeTokens))
			_, err = g.applyWithLease(ctx, actor, "zero", c.Lease.ID, reportUsage(ref(c), "opus", 0, 0))
			Expect(err).To(MatchError(domain.ErrNoTokens))
			Expect(g.usage(ctx, ref(c))).To(BeEmpty())
		})
	})

	Describe("Estimating", func() {
		BeforeEach(func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))
		})

		It("keeps the prior estimate, and the current one is the most recent", func(ctx SpecContext) {
			first := g.mustApply(ctx, actor, "est-1", estimate("#1", "opus", 1000, 100, "gut feel")).Usage[0]
			second := g.mustApply(ctx, actor, "est-2", estimate("#1", "opus", 5000, 500, "after a spike")).Usage[0]

			rows := g.usage(ctx, "#1")
			Expect(rows).To(Equal([]store.Usage{first, second}))
			Expect(rows[0].Basis).To(HaveValue(Equal("gut feel")))
			Expect(rows[1].Basis).To(HaveValue(Equal("after a spike")))
			Expect(rows[1].CreatedAt).To(BeTemporally(">=", rows[0].CreatedAt))

			Expect(*g.currentEstimate(ctx, "#1")).To(Equal(second))
			Expect(g.currentEstimate(ctx, "#2")).To(BeNil())

			By("while a rollup counts each node's current estimate alone")
			Expect(g.rollup(ctx, "#1").Estimate).To(Equal([]domain.ModelTotals{totals("opus", 1, 5000, 500)}))
		})

		It("carries the estimating principal and needs a basis", func(ctx SpecContext) {
			bob := g.principal(ctx)
			u := g.mustApply(ctx, bob, "est", estimate("#1", "", 100, 10, "comparable to #2")).Usage[0]
			Expect(u.Principal).To(Equal(bob.PrincipalID))
			Expect(u.Model).To(BeNil())
			Expect(u.LeaseID).To(BeNil())
			Expect(u.Kind).To(Equal(domain.UsageEstimate))

			_, err := g.apply(ctx, actor, "no-basis", estimate("#1", "opus", 100, 10, ""))
			Expect(err).To(MatchError(domain.ErrBasisRequired))
			Expect(g.usage(ctx, "#1")).To(HaveLen(1))
		})

		It("is denied to an agent on a node it does not own", func(ctx SpecContext) {
			agent := g.agent(ctx)
			c := g.mustClaim(ctx, agent, claimReq("claim", time.Minute))
			other := "#2"
			if c.Node.Number == 2 {
				other = "#1"
			}
			_, err := g.applyWithLease(ctx, agent, "est", c.Lease.ID, estimate(other, "opus", 100, 10, "guess"))
			Expect(err).To(MatchError(policy.ErrDenied))
			Expect(g.usage(ctx, other)).To(BeEmpty())

			By("while it may estimate the node it holds")
			r, err := g.applyWithLease(ctx, agent, "own-est", c.Lease.ID, estimate(ref(c), "opus", 100, 10, "guess"))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Usage[0].Principal).To(Equal(agent.PrincipalID))
		})
	})

	Describe("Rollups", func() {
		It("are computed over the subtree on every read, and never stored", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup",
				create("root", "Root"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "mid", Title: "Mid", Parent: "root"}},
				domain.Op{CreateNode: &domain.CreateNode{Alias: "leaf", Title: "Leaf", Parent: "mid"}},
				create("apart", "Apart"),
			)
			Expect(g.rollup(ctx, "#1")).To(Equal(&domain.Rollup{Actual: []domain.ModelTotals{}, Estimate: []domain.ModelTotals{}}))

			By("given estimates on the root and the leaf and an actual on the leaf")
			g.mustApply(ctx, actor, "estimates",
				estimate("#1", "opus", 100, 10, "plan"),
				estimate("#3", "sonnet", 200, 20, "plan"),
				estimate("#4", "opus", 999, 99, "not in the subtree"),
			)
			c := g.mustClaim(ctx, actor, claimReq("claim", time.Minute))
			Expect(c.Node.Number).To(Equal(int64(3)), "the leaf is the only ready node")
			_, err := g.applyWithLease(ctx, actor, "actual", c.Lease.ID, reportUsage("#3", "sonnet", 50, 5))
			Expect(err).NotTo(HaveOccurred())

			By("then the root's rollup sums the subtree and the leaf's rollup itself alone")
			Expect(g.rollup(ctx, "#1")).To(Equal(&domain.Rollup{
				Actual:   []domain.ModelTotals{totals("sonnet", 1, 50, 5)},
				Estimate: []domain.ModelTotals{totals("opus", 1, 100, 10), totals("sonnet", 1, 200, 20)},
			}))
			Expect(g.rollup(ctx, "#3")).To(Equal(&domain.Rollup{
				Actual:   []domain.ModelTotals{totals("sonnet", 1, 50, 5)},
				Estimate: []domain.ModelTotals{totals("sonnet", 1, 200, 20)},
			}))
			Expect(g.rollup(ctx, "#2").Estimate).To(Equal([]domain.ModelTotals{totals("sonnet", 1, 200, 20)}))

			By("and a further report is in the next read")
			_, err = g.applyWithLease(ctx, actor, "actual-2", c.Lease.ID, reportUsage("#3", "sonnet", 50, 5))
			Expect(err).NotTo(HaveOccurred())
			Expect(g.rollup(ctx, "#1").Actual).To(Equal([]domain.ModelTotals{totals("sonnet", 2, 100, 10)}))

			By("and no relation holds a total, a rate, or a currency")
			rows, err := s.Pool().Query(ctx, `
				SELECT table_name || '.' || column_name FROM information_schema.columns
				WHERE table_schema = 'public'
				  AND (column_name ~ '(total|rollup|sum|cost|price|rate|currency|usd|cents|amount|dollar)'
				       OR data_type IN ('money', 'numeric'))`)
			Expect(err).NotTo(HaveOccurred())
			columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
			Expect(err).NotTo(HaveOccurred())
			Expect(columns).To(BeEmpty())
		})

		It("reads the usage table as the documented shape: counts, never currency", func(ctx SpecContext) {
			rows, err := s.Pool().Query(ctx, `
				SELECT column_name || ':' || data_type FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'usage' ORDER BY ordinal_position`)
			Expect(err).NotTo(HaveOccurred())
			columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
			Expect(err).NotTo(HaveOccurred())
			Expect(columns).To(Equal([]string{
				"id:text", "org_id:text", "node_id:text", "lease_id:text", "principal:text", "model:text",
				"input_tokens:bigint", "output_tokens:bigint", "cache_read_tokens:bigint", "cache_write_tokens:bigint",
				"kind:text", "basis:text", "created_at:timestamp with time zone",
			}))
		})

		It("rejects an unknown node", func(ctx SpecContext) {
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := domain.SubtreeRollup(ctx, tx, g.org, domain.NewULID())
				Expect(err).To(MatchError(domain.ErrNodeNotFound))
				_, err = domain.ListUsage(ctx, tx, g.org, domain.NewULID())
				Expect(err).To(MatchError(domain.ErrNodeNotFound))
				_, err = domain.CurrentEstimate(ctx, tx, g.org, domain.NewULID())
				return err
			})).To(MatchError(domain.ErrNodeNotFound))
		})
	})
})
