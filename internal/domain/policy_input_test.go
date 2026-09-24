package domain_test

import (
	"fmt"
	"testing/fstest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// bundle compiles one policy module for a spec that pins what the input
// to a policy looks like, rather than what the default bundle decides.
func bundle(ctx SpecContext, rules string) *policy.Bundle {
	GinkgoHelper()
	b, err := policy.Load(ctx, fstest.MapFS{"narrow.rego": &fstest.MapFile{Data: []byte(
		"package skift.authz\n\ndefault allow := false\n\n" + rules + "\n\ndecision := {\"allow\": allow, \"reasons\": []}\n")}})
	Expect(err).NotTo(HaveOccurred())
	return b
}

// The default bundle decides most operations from the proposed state, so
// these specs bring bundles that decide from the parts of the input it
// leaves alone: the node as it stood, the comment as the operation leaves
// it, and the lease an operation carries.
var _ = Describe("The policy's view of an operation", Label(testsupport.DBLabel), func() {
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

	It("shows a moved node's lineage as it stood, whichever side of the move is the root", func(ctx SpecContext) {
		By("given R and U at the root, C under R, and a policy that lets a node be updated only while it stands under R")
		g.mustApply(ctx, user, "layout", create("r", "R"), create("u", "U"), childOf("c", "C", "r"))
		underR := bundle(ctx, fmt.Sprintf(`
allow if {
	input.action == "update_node"
	input.resource.node.lineage[_].id == %q
}`, g.id("#1")))
		move := func(requestID, parent string) error {
			GinkgoHelper()
			_, err := domain.Apply(ctx, s, underR, g.org, user, domain.Patch{RequestID: requestID, Project: "platform",
				Ops: []domain.Op{{UpdateNode: &domain.UpdateNode{Node: "#3", Parent: &parent}}}})
			return err
		}

		By("when C moves from under R to the root, the policy sees it under R and permits the move")
		Expect(move("to-root", "")).To(Succeed())
		Expect(g.getNode(ctx, "#3").ParentID).To(BeNil())

		By("and when C moves from the root under U, or back under R, the policy sees it at the root and refuses")
		Expect(move("to-u", "#2")).To(BeDeniedAt(0))
		Expect(move("to-r", "#1")).To(BeDeniedAt(0), "where it is going does not change where it stood")
		Expect(g.getNode(ctx, "#3").ParentID).To(BeNil())

		By("and when C moves from under R to under U, the policy sees it under R and permits the move")
		g.mustApply(ctx, user, "back", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#3", Parent: ptr("#1")}})
		Expect(move("to-u-from-r", "#2")).To(Succeed())
		Expect(g.getNode(ctx, "#3").ParentID).To(HaveValue(Equal(g.id("#2"))))
	})

	It("shows a comment as the operation leaves it, resolved or not", func(ctx SpecContext) {
		By("given a policy that lets a comment be edited while open, and resolved by the operation that leaves it resolved")
		g.mustApply(ctx, user, "layout", create("one", "Node 1"))
		open := bundle(ctx, `
allow if input.action == "add_comment"

allow if {
	input.action == "edit_comment"
	input.resource.comment.resolved == false
	input.after.comment.resolved == false
}

allow if {
	input.action == "resolve_comment"
	input.resource.comment.resolved == false
	input.after.comment.resolved == true
}`)
		apply := func(requestID string, op domain.Op) (*domain.Result, error) {
			GinkgoHelper()
			return domain.Apply(ctx, s, open, g.org, user, domain.Patch{RequestID: requestID, Project: "platform", Ops: []domain.Op{op}})
		}

		By("when a comment is added, edited, and resolved under it")
		added, err := apply("add", addComment("c", "#1", "first draft"))
		Expect(err).NotTo(HaveOccurred())
		id := added.CommentAliases["c"]
		_, err = apply("edit", editComment(id, "second draft"))
		Expect(err).NotTo(HaveOccurred(), "an unresolved comment reads as such")
		_, err = apply("resolve", resolveComment(id))
		Expect(err).NotTo(HaveOccurred(), "a resolution reads as resolved")

		By("then the comment is resolved with its last edit")
		h := g.readComment(ctx, id)
		Expect(h.Comment.Body).To(Equal("second draft"))
		Expect(h.Comment.ResolvedAt).NotTo(BeNil())
	})

	It("audits a refusal the policy gives no reason for", func(ctx SpecContext) {
		By("given a policy that refuses everything and says nothing")
		mute := bundle(ctx, "")

		By("when a patch is refused under it")
		_, err := domain.Apply(ctx, s, mute, g.org, user, domain.Patch{RequestID: "mute", Project: "platform", Ops: []domain.Op{create("one", "Node 1")}})

		By("then the refusal is the whole error, and it is on record with no reasons")
		Expect(err).To(BeDeniedAt(0))
		Expect(err.Error()).NotTo(ContainSubstring("audit"))
		denials := g.denials(ctx)
		Expect(denials).To(HaveLen(1))
		Expect(denials[0].Op).To(Equal(domain.OpCreateNode))
		Expect(denials[0].Reasons).To(BeEmpty())
	})

	It("carries the lease a heartbeat or release acts on, and none on a claim", func(ctx SpecContext) {
		By("given a policy that lets a lease be renewed and released only as the lease the request carries")
		g.mustApply(ctx, user, "layout", create("one", "Node 1"))
		agent := g.agent(ctx)
		carried := bundle(ctx, `
allow if {
	input.action == "claim_node"
	input.lease == null
}

allow if {
	input.action in {"heartbeat_lease", "release_lease"}
	input.lease.id == input.resource.lease.id
	input.lease.node_id == input.resource.node.id
	input.lease.holder == input.principal.id
}`)

		By("when an agent claims, renews, and releases under it")
		claimed, err := domain.Claim(ctx, s, carried, g.org, agent, claimReq("claim", time.Minute))
		Expect(err).NotTo(HaveOccurred(), "a claim carries no lease: it makes one")
		Expect(claimed).NotTo(BeNil())
		_, err = domain.Heartbeat(ctx, s, carried, g.org, agent, "beat", claimed.Lease.ID, 0)
		Expect(err).NotTo(HaveOccurred(), "a heartbeat carries the lease it renews")
		released, err := domain.Release(ctx, s, carried, g.org, agent, "release", claimed.Lease.ID, domain.ReleaseComplete, "")
		Expect(err).NotTo(HaveOccurred(), "a release carries the lease it ends")

		By("then the lease is over")
		Expect(released.Lease.ReleasedAt).NotTo(BeNil())
	})
})
