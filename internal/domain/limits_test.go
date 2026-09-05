package domain_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// asLimitError asserts that err is, or wraps, a [*domain.LimitError]
// naming limit, and hands it back for closer inspection.
func asLimitError(err error, limit domain.Limit) domain.LimitError {
	GinkgoHelper()
	var limitErr *domain.LimitError
	Expect(errors.As(err, &limitErr)).To(BeTrue(), "expected a limit error, got %v", err)
	Expect(limitErr.Limit).To(Equal(limit))
	Expect(err.Error()).To(ContainSubstring(string(limit)))
	return *limitErr
}

// creates is n create_node operations, each with a distinct alias.
func creates(n int) []domain.Op {
	ops := make([]domain.Op, 0, n)
	for i := range n {
		ops = append(ops, create(fmt.Sprintf("n%d", i), fmt.Sprintf("Node %d", i)))
	}
	return ops
}

var _ = Describe("Limits", func() {
	Describe("Configuration", Label("unit"), func() {
		It("is in force by default and refuses a ceiling at zero", func() {
			Expect(domain.ActiveLimits()).To(Equal(domain.DefaultLimits()))

			bad := domain.DefaultLimits()
			bad.PageSize = 0
			Expect(domain.SetLimits(bad)).To(MatchError(domain.ErrInvalidLimits))
			Expect(domain.ActiveLimits()).To(Equal(domain.DefaultLimits()))
		})

		It("bounds every ceiling the domain model lists", func() {
			l := domain.DefaultLimits()
			Expect(l.OpsPerPatch).To(BeNumerically(">", 0))
			Expect(l.NodesPerPatch).To(BeNumerically(">", 0))
			Expect(l.EdgesPerPatch).To(BeNumerically(">", 0))
			Expect(l.BodyBytes).To(BeNumerically(">", 0))
			Expect(l.ResultBytes).To(BeNumerically(">", 0))
			Expect(l.CommentBytes).To(BeNumerically(">", 0))
			Expect(l.InlinePartBytes).To(BeNumerically(">", 0))
			Expect(l.BlobBytes).To(BeNumerically(">", 0))
			Expect(l.LeaseTTL).To(BeNumerically(">", 0))
			Expect(l.PageSize).To(BeNumerically(">", 0))
		})

		It("reports the limit, the ceiling, and the value", func() {
			err := &domain.LimitError{Limit: domain.LimitOpsPerPatch, Max: 3, Value: 4}
			Expect(err.Error()).To(SatisfyAll(
				ContainSubstring("operations per patch"),
				ContainSubstring("4"),
				ContainSubstring("3"),
			))
		})
	})

	Describe("Enforcement", Label(testsupport.DBLabel), func() {
		var (
			s      *store.Store
			g      *graph
			actor  domain.Actor
			limits domain.Limits
		)

		BeforeEach(func(ctx SpecContext) {
			s = store.FromPool(pg.Database(ctx))
			g = newGraph(ctx, s)
			actor = g.principal(ctx)
			limits = domain.ActiveLimits()
		})

		// nothingWritten asserts the request left no trace: no node, no
		// event, and no idempotency key.
		nothingWritten := func(ctx context.Context, requestID string) {
			GinkgoHelper()
			Expect(g.countNodes(ctx)).To(BeZero())
			Expect(g.countEvents(ctx, requestID)).To(BeZero())
			_, err := s.Queries().GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{OrgID: g.org, RequestID: requestID})
			Expect(err).To(MatchError(pgx.ErrNoRows))
		}

		Describe("Scenarios", func() {
			It("An oversized patch fails cheaply", func(ctx SpecContext) {
				By("given a patch with one more operation than the ceiling allows")
				ops := creates(limits.OpsPerPatch + 1)

				By("when it is applied")
				_, err := g.apply(ctx, actor, "oversized", ops...)

				By("then it is rejected before any row is written")
				Expect(err).To(HaveOccurred())
				nothingWritten(ctx, "oversized")

				By("and the error names the limit")
				limitErr := asLimitError(err, domain.LimitOpsPerPatch)
				Expect(limitErr.Max).To(Equal(int64(limits.OpsPerPatch)))
				Expect(limitErr.Value).To(Equal(int64(limits.OpsPerPatch + 1)))
			})

			It("Limits are not rate limits", func(ctx SpecContext) {
				By("given a principal applying many small patches in quick succession")
				const patches = 200
				start := time.Now()
				for i := range patches {
					_, err := g.apply(ctx, actor, fmt.Sprintf("burst-%d", i), create(fmt.Sprintf("b%d", i), "Small"))

					By("then none is rejected for frequency")
					Expect(err).NotTo(HaveOccurred())
				}
				Expect(g.countNodes(ctx)).To(Equal(patches))
				GinkgoWriter.Printf("%d patches in %s\n", patches, time.Since(start))
			})
		})

		It("bounds the nodes a patch creates, under configured ceilings", func(ctx SpecContext) {
			// Each create is one operation, so the node ceiling can only
			// fire when it sits below the operation ceiling.
			lowered := limits
			lowered.NodesPerPatch = 3
			Expect(domain.SetLimits(lowered)).To(Succeed())
			DeferCleanup(func() { Expect(domain.SetLimits(limits)).To(Succeed()) })

			_, err := g.apply(ctx, actor, "nodes", creates(4)...)
			limitErr := asLimitError(err, domain.LimitNodesPerPatch)
			Expect(limitErr.Max).To(Equal(int64(3)))
			Expect(limitErr.Value).To(Equal(int64(4)))
			nothingWritten(ctx, "nodes")

			By("and three is still fine")
			g.mustApply(ctx, actor, "three", creates(3)...)
			Expect(g.countNodes(ctx)).To(Equal(3))
		})

		It("bounds the edges a patch adds, counting blocked_by", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))

			blockers := make([]string, 0, limits.EdgesPerPatch+1)
			for range limits.EdgesPerPatch + 1 {
				blockers = append(blockers, "#1")
			}
			_, err := g.apply(ctx, actor, "edges", domain.Op{CreateNode: &domain.CreateNode{Title: "B", BlockedBy: blockers}})
			limitErr := asLimitError(err, domain.LimitEdgesPerPatch)
			Expect(limitErr.Value).To(Equal(int64(limits.EdgesPerPatch + 1)))
			Expect(g.countNodes(ctx)).To(Equal(1))
			Expect(g.countEvents(ctx, "edges")).To(BeZero())
		})

		It("bounds a node body on create and update", func(ctx SpecContext) {
			big := strings.Repeat("x", limits.BodyBytes+1)
			_, err := g.apply(ctx, actor, "create-body", domain.Op{CreateNode: &domain.CreateNode{Title: "A", Body: big}})
			asLimitError(err, domain.LimitBodyBytes)
			var opErr *domain.OpError
			Expect(errors.As(err, &opErr)).To(BeTrue(), "expected an operation error, got %v", err)
			Expect(opErr.Op).To(Equal(domain.OpCreateNode))
			nothingWritten(ctx, "create-body")

			g.mustApply(ctx, actor, "setup", create("a", "A"))
			_, err = g.apply(ctx, actor, "update-body", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#1", Body: &big}})
			asLimitError(err, domain.LimitBodyBytes)
			Expect(g.countEvents(ctx, "update-body")).To(BeZero())
		})

		It("clamps a lease TTL to the ceiling", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))
			c := g.mustClaim(ctx, actor, claimReq("long", 2*limits.LeaseTTL))
			Expect(c.Lease.ExpiresAt).To(BeTemporally("~", time.Now().Add(limits.LeaseTTL), 5*time.Second))
		})

		It("bounds a page of ready nodes and of expired leases", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))

			err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := domain.Ready(ctx, tx, g.org, domain.ReadyQuery{Projects: []string{g.project}, Limit: limits.PageSize + 1})
				return err
			})
			asLimitError(err, domain.LimitPageSize)

			err = s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				nodes, err := domain.Ready(ctx, tx, g.org, domain.ReadyQuery{Projects: []string{g.project}})
				Expect(nodes).To(HaveLen(1))
				return err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = domain.ExpireLeases(ctx, s, pol, g.org, g.principalOf(ctx, "service"), limits.PageSize+1)
			asLimitError(err, domain.LimitPageSize)
		})
	})
})
