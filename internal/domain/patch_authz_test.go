package domain_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// dropCascade is a drop_node operation taking the subtree the node blocks.
func dropCascade(node, reason string) domain.Op {
	return domain.Op{DropNode: &domain.DropNode{Node: node, Reason: reason, Cascade: &domain.Cascade{MaxNodes: 10}}}
}

// denials reads the audit of one organization's refusals, in order.
func (g *graph) denials(ctx context.Context) []store.PolicyDenial {
	GinkgoHelper()
	rows, err := g.s.Queries().ListPolicyDenialsAfter(ctx, store.ListPolicyDenialsAfterParams{OrgID: g.org, AfterID: "", PageSize: 100})
	Expect(err).NotTo(HaveOccurred())
	return rows
}

// asDenial is the [*domain.Denial] an error carries, or nil.
func asDenial(err error) *domain.Denial {
	var d *domain.Denial
	if !errors.As(err, &d) {
		return nil
	}
	return d
}

var _ = Describe("Authorization of the resolved patch", Label(testsupport.DBLabel), func() {
	var (
		s    *store.Store
		g    *graph
		user domain.Actor
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		user = g.principal(ctx)
	})

	Describe("Scenarios", func() {
		It("A cascade cannot reach unauthorized work", func(ctx SpecContext) {
			By("given node 1 blocks node 2, and the caller may not write node 2")
			g.mustApply(ctx, user, "layout", create("one", "Node 1"), create("two", "Node 2"), addEdge("one", "two"))
			agent := g.agent(ctx)
			held := g.mustClaim(ctx, agent, claimReq("claim-1", 0))
			Expect(held.Node.Number).To(Equal(int64(1)), "the agent holds node 1 and nothing else")

			By("when the caller drops node 1 with cascade")
			_, err := g.applyWithLease(ctx, agent, "axe", held.Lease.ID, dropCascade("#1", domain.DropNotViable))

			By("then the whole patch is denied")
			Expect(err).To(BeDeniedAt(0))
			Expect(err).To(MatchError(ContainSubstring("no rule permits agent drop_node")))
			denial := asDenial(err)
			Expect(denial).NotTo(BeNil())
			Expect(denial.Op).To(Equal(domain.OpDropNode))
			Expect(denial.NodeID).To(Equal(g.id("#2")), "the refused operation is the cascade's drop of node 2, which the client never wrote")
			Expect(g.getNode(ctx, "#2").Status).To(Equal("open"))

			By("and node 1 is not dropped")
			Expect(g.getNode(ctx, "#1").Status).To(Equal("open"))
			Expect(g.countEvents(ctx, "axe")).To(BeZero())
		})

		It("An alias cannot smuggle a write", func(ctx SpecContext) {
			By("given node 1 with children 2 and 3, and an agent holding node 2")
			g.mustApply(ctx, user, "layout", create("root", "Node 1"), childOf("two", "Node 2", "root"), childOf("three", "Node 3", "root"))
			agent := g.agent(ctx)
			held := g.mustClaim(ctx, agent, claimReq("claim-2", 0))
			Expect(held.Node.Number).To(Equal(int64(2)))
			nodes, edges := g.countNodes(ctx), g.countEdges(ctx)

			By("given a patch whose later operation targets a node the caller may not touch")
			ops := []domain.Op{
				childOf("pre", "Prerequisite", "#2"),
				addEdge("pre", "#3"),
			}

			By("when the patch is applied")
			_, err := g.applyWithLease(ctx, agent, "smuggle", held.Lease.ID, ops...)

			By("then nothing in the patch commits")
			Expect(err).To(BeDeniedAt(1), "the first operation was allowed on its own; the second is what the patch is refused for")
			Expect(asDenial(err).NodeID).To(Equal(g.id("#3")))
			Expect(g.countNodes(ctx)).To(Equal(nodes), "the node the alias named was rolled back with the rest")
			Expect(g.countEdges(ctx)).To(Equal(edges))
			Expect(g.countEvents(ctx, "smuggle")).To(BeZero())
			Expect(g.explain(ctx, "#3").Reasons).NotTo(ContainElement(BeReason(domain.ReasonBlocked, 4)))
		})

		It("Authorization sees the proposed state", func(ctx SpecContext) {
			// A node's project is fixed: parents are scoped to the project
			// by nodes_parent_fkey and no operation rewrites project_id.
			// The move the model does allow is between subtrees, and the
			// same question arises: the node as it stands is the caller's
			// to write, and the node as proposed is not.
			By("given node 1 with children 2 and 3, and an agent holding node 2 with node 4 created under it")
			g.mustApply(ctx, user, "layout", create("root", "Node 1"), childOf("two", "Node 2", "root"), childOf("three", "Node 3", "root"))
			agent := g.agent(ctx)
			held := g.mustClaim(ctx, agent, claimReq("claim-2", 0))
			Expect(held.Node.Number).To(Equal(int64(2)))
			_, err := g.applyWithLease(ctx, agent, "child", held.Lease.ID, childOf("child", "Node 4", "#2"))
			Expect(err).NotTo(HaveOccurred(), "node 4 is the agent's own, so as it stands it may write it")

			By("given a patch that would move node 4 under node 3, which the agent cannot write")
			parent := "#3"
			move := domain.Op{UpdateNode: &domain.UpdateNode{Node: "#4", Parent: &parent}}
			_, err = g.applyWithLease(ctx, agent, "move", held.Lease.ID, move)

			By("then the patch is denied")
			Expect(err).To(BeDeniedAt(0), "the node as it stands is the agent's; the node as proposed is not")
			Expect(err).To(MatchError(ContainSubstring("no rule permits agent update_node")))
			Expect(asDenial(err).NodeID).To(Equal(g.id("#4")))
			Expect(g.getNode(ctx, "#4").ParentID).To(HaveValue(Equal(g.id("#2"))))
			Expect(g.countEvents(ctx, "move")).To(BeZero())

			By("and a user, whom the policy lets write anywhere in the organization, may make the same move")
			_, err = g.apply(ctx, user, "move-by-user", move)
			Expect(err).NotTo(HaveOccurred())
			Expect(g.getNode(ctx, "#4").ParentID).To(HaveValue(Equal(g.id("#3"))))
		})
	})

	Describe("Cascades", func() {
		It("authorize every drop they produce, and record each", func(ctx SpecContext) {
			By("given node 1 blocks nodes 2 and 3, and node 3 blocks node 4")
			g.mustApply(ctx, user, "layout",
				create("one", "Node 1"), create("two", "Node 2"), create("three", "Node 3"), create("four", "Node 4"),
				addEdge("one", "two"), addEdge("one", "three"), addEdge("three", "four"))

			By("when a user drops node 1 with cascade")
			r, err := g.apply(ctx, user, "axe", dropCascade("#1", domain.DropSuperseded))
			Expect(err).NotTo(HaveOccurred())

			By("then nodes 1 through 4 are dropped, each by an authorized, recorded operation")
			for _, ref := range []string{"#1", "#2", "#3", "#4"} {
				Expect(g.getNode(ctx, ref).Status).To(Equal("dropped"), ref)
			}
			Expect(r.Events).To(HaveLen(4))
			Expect(ops(r.Events)).To(HaveEach(domain.OpDropNode))
			Expect(r.Events).To(versioned())
			Expect(r.Events).To(HaveEach(HaveField("Reason", HaveValue(Equal(domain.DropSuperseded)))))
			Expect(g.denials(ctx)).To(BeEmpty())
		})
	})

	Describe("Denials", func() {
		var (
			agent domain.Actor
			lease store.Lease
		)

		BeforeEach(func(ctx SpecContext) {
			By("given node 1 with children 2 and 3, and an agent holding node 2")
			g.mustApply(ctx, user, "layout", create("root", "Node 1"), childOf("two", "Node 2", "root"), childOf("three", "Node 3", "root"))
			agent = g.agent(ctx)
			held := g.mustClaim(ctx, agent, claimReq("claim-2", 0))
			Expect(held.Node.Number).To(Equal(int64(2)))
			lease = held.Lease
		})

		It("roll back the entire patch, including what was allowed before them", func(ctx SpecContext) {
			nodes := g.countNodes(ctx)

			By("when a patch is allowed at its first operation and denied at its second")
			_, err := g.applyWithLease(ctx, agent, "partial", lease.ID, childOf("c", "Child", "#2"), update("#3", "Edited"))
			Expect(err).To(BeDeniedAt(1))

			By("then nothing of it remains")
			Expect(g.countNodes(ctx)).To(Equal(nodes))
			Expect(g.getNode(ctx, "#3").Title).To(Equal("Node 3"))
			Expect(g.countEvents(ctx, "partial")).To(BeZero())

			By("and the request id was released with it, so a corrected patch may reuse it")
			r, err := g.applyWithLease(ctx, agent, "partial", lease.ID, childOf("c", "Child", "#2"))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Replayed).To(BeFalse())
			Expect(r.Nodes).To(HaveLen(1))
		})

		It("are audited with the policy bundle version", func(ctx SpecContext) {
			By("when the agent is refused an operation")
			_, err := g.applyWithLease(ctx, agent, "sibling", lease.ID, update("#3", "Edited by the agent"))
			Expect(err).To(BeDeniedAt(0))

			By("then the refusal is on record, outside the transaction it rolled back")
			denials := g.denials(ctx)
			Expect(denials).To(HaveLen(1))
			d := denials[0]
			Expect(d.PolicyVersion).To(Equal(pol.Version()))
			Expect(d.Op).To(Equal(domain.OpUpdateNode))
			Expect(d.NodeID).To(HaveValue(Equal(g.id("#3"))))
			Expect(d.ProjectID).To(HaveValue(Equal(g.project)))
			Expect(d.PrincipalID).To(Equal(agent.PrincipalID))
			Expect(d.PrincipalKind).To(Equal("agent"))
			Expect(d.IdentityScheme).To(Equal(agent.IdentityScheme))
			Expect(d.RequestID).To(HaveValue(Equal("sibling")))
			Expect(d.Reasons).To(ConsistOf("no rule permits agent update_node"))
			Expect(g.countEvents(ctx, "sibling")).To(BeZero(), "a denial is not a mutation, so it is not an event")

			By("and a denial for want of any bundle records that there was none")
			var none *policy.Bundle
			_, err = domain.Apply(ctx, s, none, g.org, user, domain.Patch{RequestID: "no-policy", Project: "platform", Ops: []domain.Op{create("x", "X")}})
			Expect(err).To(BeDeniedAt(0))
			denials = g.denials(ctx)
			Expect(denials).To(HaveLen(2))
			Expect(denials[1].PolicyVersion).To(BeEmpty())
			Expect(denials[1].Op).To(Equal(domain.OpCreateNode))
			Expect(denials[1].Reasons).To(ConsistOf(ContainSubstring("no policy bundle is loaded")))

			By("and the record cannot be rewritten")
			_, err = s.Pool().Exec(ctx, `DELETE FROM policy_denials WHERE id = $1`, d.ID)
			Expect(err).To(MatchError(ContainSubstring("append-only")))
		})
	})
})
