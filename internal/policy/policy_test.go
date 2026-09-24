package policy_test

import (
	"errors"
	"testing/fstest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/policy"
)

// Fixtures follow the scenarios: agent A holds a lease on node 7, whose
// parent is node 1 and whose sibling is node 8. A user laid all three out.

var (
	agentA = policy.Principal{ID: "A", Kind: "agent"}
	agentB = policy.Principal{ID: "B", Kind: "agent"}
	user   = policy.Principal{ID: "U", Kind: "user"}
	leaseA = &policy.Lease{ID: "L7", NodeID: "n7", Holder: "A"}
	node1  = node("n1", nil, "U", nil)
	node7  = node("n7", &node1.ID, "U", node1.Lineage)
	node8  = node("n8", &node1.ID, "U", node1.Lineage)
)

// node is a node in project p1 as policy sees it, with its lineage built
// from its creator and its parent's lineage.
func node(id string, parent *string, creator string, ancestors []policy.Ancestor) policy.Node {
	lineage := append([]policy.Ancestor{{ID: id, CreatedBy: creator}}, ancestors...)
	return policy.Node{ID: id, ProjectID: "p1", ParentID: parent, Kind: "task", Status: "open", CreatedBy: creator, Lineage: lineage}
}

// childOf is a node creator made under parent.
func childOf(id string, parent policy.Node, creator string) policy.Node {
	return node(id, &parent.ID, creator, parent.Lineage)
}

// gate makes n a gate: user-assigned.
func gate(n policy.Node) policy.Node {
	kind := "user"
	n.AssigneeKind = &kind
	return n
}

func withStatus(n policy.Node, status string) policy.Node {
	n.Status = status
	return n
}

// nodeOp is an action on a node moving it from before to after.
func nodeOp(p policy.Principal, action string, before, after *policy.Node, lease *policy.Lease) policy.Input {
	return policy.Input{Principal: p, Action: action, Resource: policy.Resource{Node: before}, After: &policy.Resource{Node: after}, Lease: lease}
}

func addEdge(p policy.Principal, from, to policy.Node, lease *policy.Lease) policy.Input {
	e := &policy.Edge{From: from, To: to, Kind: "blocks"}
	return policy.Input{Principal: p, Action: "add_edge", Resource: policy.Resource{Edge: e}, After: &policy.Resource{Edge: e}, Lease: lease}
}

// claim is a claim_node action on a node, with the lease it would take.
func claim(p policy.Principal, n policy.Node) policy.Input {
	return policy.Input{
		Principal: p, Action: "claim_node",
		Resource: policy.Resource{Node: &n, Lease: &policy.Lease{ID: "L" + n.ID, NodeID: n.ID, Holder: p.ID}},
	}
}

func removeEdge(p policy.Principal, from, to policy.Node, lease *policy.Lease) policy.Input {
	e := &policy.Edge{From: from, To: to, Kind: "blocks"}
	return policy.Input{Principal: p, Action: "remove_edge", Resource: policy.Resource{Edge: e}, Lease: lease}
}

// BeDenied matches a policy refusal.
func BeDenied() OmegaMatcher {
	return MatchError(policy.ErrDenied)
}

var _ = Describe("Policy", Label("unit"), func() {
	var bundle *policy.Bundle

	BeforeEach(func(ctx SpecContext) {
		var err error
		bundle, err = policy.LoadDefault(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("Scenarios", func() {
		It("An agent cannot edit a sibling", func(ctx SpecContext) {
			By("given agent A holds a lease on node 7, and node 8 shares node 7's parent")
			By("when agent A patches node 8")
			retitled := node8
			retitled.Kind = "chore"
			err := bundle.Authorize(ctx, nodeOp(agentA, "update_node", &node8, &retitled, leaseA))

			By("then the patch is denied")
			Expect(err).To(BeDenied())
			Expect(err).To(MatchError(ContainSubstring("no rule permits agent update_node")))
		})

		It("An agent can block itself", func(ctx SpecContext) {
			By("given agent A holds a lease on node 7")
			By("when it creates a node and adds an edge from it into node 7")
			created := childOf("n9", node7, "A")
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "create_node", nil, &created, leaseA))).To(Succeed())
			err := bundle.Authorize(ctx, addEdge(agentA, created, node7, leaseA))

			By("then the patch is allowed")
			Expect(err).To(Succeed())
		})

		It("An agent cannot bypass its own gate", func(ctx SpecContext) {
			By("given agent A filed gate node 9 blocking node 7")
			node9 := gate(childOf("n9", node7, "A"))

			By("when agent A drops node 9")
			err := bundle.Authorize(ctx, nodeOp(agentA, "drop_node", &node9, ptr(withStatus(node9, "dropped")), leaseA))

			By("then the operation is denied")
			Expect(err).To(BeDenied())
			Expect(err).To(MatchError(ContainSubstring("filed gate n9 may not drop it: a person decides it; comment what you found on it and release your lease with reason gate")))

			By("and when agent A completes node 9")
			err = bundle.Authorize(ctx, nodeOp(agentA, "complete_node", &node9, ptr(withStatus(node9, "done")), leaseA))

			By("then the operation is denied")
			Expect(err).To(BeDenied())
			Expect(err).To(MatchError(ContainSubstring("filed gate n9 may not complete it: a person decides it; comment what you found on it and release your lease with reason gate")))
		})

		It("Policy failure is closed", func(ctx SpecContext) {
			By("given a policy bundle that fails to compile")
			broken, err := policy.Load(ctx, fstest.MapFS{"skift.rego": {Data: []byte("package skift.authz\n\nallow if {\n")}})
			Expect(err).To(MatchError(ContainSubstring("compile policy bundle")))
			Expect(broken).To(BeNil())

			By("when any authorization is evaluated")
			for _, in := range []policy.Input{
				nodeOp(user, "update_node", &node7, &node7, nil),
				nodeOp(agentA, "update_node", &node7, &node7, leaseA),
				{Principal: user, Action: "expire_lease"},
			} {
				By("then it is denied")
				err := broken.Authorize(ctx, in)
				Expect(err).To(BeDenied())
				Expect(err).To(MatchError(ContainSubstring("no policy bundle is loaded")))
			}
		})
	})

	Describe("A lease holder", func() {
		It("may mutate its own node", func(ctx SpecContext) {
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "update_node", &node7, &node7, leaseA))).To(Succeed())
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "drop_node", &node7, ptr(withStatus(node7, "dropped")), leaseA))).To(Succeed())
		})

		It("may mutate the children it creates under its node, not arbitrary descendants", func(ctx SpecContext) {
			mine := childOf("n9", node7, "A")
			deeper := childOf("n10", mine, "A")
			theirs := childOf("n11", node7, "B")
			underTheirs := childOf("n12", theirs, "A")

			Expect(bundle.Authorize(ctx, nodeOp(agentA, "create_node", nil, &mine, leaseA))).To(Succeed())
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "update_node", &mine, &mine, leaseA))).To(Succeed())
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "update_node", &deeper, &deeper, leaseA))).To(Succeed())
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "update_node", &theirs, &theirs, leaseA))).To(BeDenied(), "a child someone else created")
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "update_node", &underTheirs, &underTheirs, leaseA))).To(BeDenied(), "a node of its own below one someone else created")
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "update_node", &node1, &node1, leaseA))).To(BeDenied(), "its parent")
		})

		It("may not mutate without a lease, or with someone else's", func(ctx SpecContext) {
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "update_node", &node7, &node7, nil))).To(BeDenied())
			Expect(bundle.Authorize(ctx, nodeOp(agentB, "update_node", &node7, &node7, leaseA))).To(BeDenied())
		})

		It("may file work without a lease, but not mutate what it filed", func(ctx SpecContext) {
			root := node("n9", nil, "A", nil)
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "create_node", nil, &root, nil))).To(Succeed(), "a root node is filing, not mutation")
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "update_node", &root, &root, nil))).To(BeDenied(), "changing it afterwards needs a lease")
			underTheirs := node("n10", &node1.ID, "A", node1.Lineage)
			Expect(bundle.Authorize(ctx, nodeOp(agentA, "create_node", nil, &underTheirs, nil))).To(BeDenied(), "another principal's lineage is not filing")
			Expect(bundle.Authorize(ctx, addEdge(agentA, node8, root, nil))).To(Succeed(), "filed work may wait on anything it can see")
			Expect(bundle.Authorize(ctx, addEdge(agentA, root, node7, nil))).To(BeDenied(), "but may not make someone else's work wait")
		})

		It("may add blocking edges into its own node", func(ctx SpecContext) {
			Expect(bundle.Authorize(ctx, addEdge(agentA, node8, node7, leaseA))).To(Succeed())
			Expect(bundle.Authorize(ctx, addEdge(agentA, node1, node7, leaseA))).To(Succeed())
			Expect(bundle.Authorize(ctx, removeEdge(agentA, node8, node7, leaseA))).To(Succeed())
		})

		It("needs link permission on the target of an edge out of its node", func(ctx SpecContext) {
			Expect(bundle.Authorize(ctx, addEdge(agentA, node7, node8, leaseA))).To(BeDenied(), "a sibling is not its to block")
			mine := childOf("n9", node7, "A")
			Expect(bundle.Authorize(ctx, addEdge(agentA, node7, mine, leaseA))).To(Succeed(), "its own child is")
			Expect(bundle.Authorize(ctx, addEdge(agentA, node8, node1, leaseA))).To(BeDenied(), "two nodes not its own")
			Expect(bundle.Authorize(ctx, removeEdge(agentA, node7, node8, leaseA))).To(BeDenied())
		})
	})

	Describe("Cross-project edges", func() {
		elsewhere := node("x1", nil, "U", nil)
		elsewhere.ProjectID = "p2"

		It("require write on the source and link on the target", func(ctx SpecContext) {
			Expect(bundle.Authorize(ctx, addEdge(user, elsewhere, node7, nil))).To(Succeed(), "a user has both")
			Expect(bundle.Authorize(ctx, addEdge(agentA, elsewhere, node7, leaseA))).To(BeDenied(), "no write on the source")
			Expect(bundle.Authorize(ctx, addEdge(agentA, node7, elsewhere, leaseA))).To(BeDenied(), "no link on the target")
		})
	})

	Describe("Gates", func() {
		node9 := gate(childOf("n9", node7, "A"))
		done := ptr(withStatus(node9, "done"))

		It("Only a user principal may complete a gate", func(ctx SpecContext) {
			Expect(bundle.Authorize(ctx, nodeOp(user, "complete_node", &node9, done, nil))).To(Succeed())
			leaseB := &policy.Lease{ID: "L9", NodeID: "n9", Holder: "B"}
			err := bundle.Authorize(ctx, nodeOp(agentB, "complete_node", &node9, done, leaseB))
			Expect(err).To(BeDenied())
			Expect(err).To(MatchError(ContainSubstring("only a user may complete gate n9: a person completes it; comment your recommendation on it and release your lease with reason gate")))
			Expect(bundle.Authorize(ctx, nodeOp(policy.Principal{ID: "S", Kind: "service"}, "complete_node", &node9, done, nil))).To(BeDenied())
		})

		It("An agent may not claim a gate", func(ctx SpecContext) {
			Expect(bundle.Authorize(ctx, claim(agentB, node9))).To(BeDenied())
			Expect(bundle.Authorize(ctx, claim(agentB, node8))).To(Succeed(), "an ordinary node is claimable")
			Expect(bundle.Authorize(ctx, claim(user, node9))).To(Succeed(), "a person may take the gate it will perform")
		})

		It("An agent that filed a gate may not drop, complete, reassign, reclassify, or unlink it", func(ctx SpecContext) {
			filer := agentA
			Expect(bundle.Authorize(ctx, nodeOp(filer, "drop_node", &node9, ptr(withStatus(node9, "dropped")), leaseA))).To(BeDenied())
			Expect(bundle.Authorize(ctx, nodeOp(filer, "complete_node", &node9, done, leaseA))).To(BeDenied())

			reassigned := node9
			reassigned.Assignee = ptr("U")
			Expect(bundle.Authorize(ctx, nodeOp(filer, "update_node", &node9, &reassigned, leaseA))).To(BeDenied())
			reclassified := node9
			reclassified.AssigneeKind = ptr("agent")
			Expect(bundle.Authorize(ctx, nodeOp(filer, "update_node", &node9, &reclassified, leaseA))).To(BeDenied())
			relabelled := node9
			relabelled.Kind = "chore"
			Expect(bundle.Authorize(ctx, nodeOp(filer, "update_node", &node9, &relabelled, leaseA))).To(BeDenied())
			Expect(bundle.Authorize(ctx, removeEdge(filer, node9, node7, leaseA))).To(BeDenied())

			By("while it may still reword it, and a user may dispose of it")
			reworded := node9
			reworded.Kind = "task"
			Expect(bundle.Authorize(ctx, nodeOp(filer, "update_node", &node9, &reworded, leaseA))).To(Succeed())
			Expect(bundle.Authorize(ctx, nodeOp(user, "drop_node", &node9, ptr(withStatus(node9, "dropped")), nil))).To(Succeed())
			Expect(bundle.Authorize(ctx, removeEdge(user, node9, node7, nil))).To(Succeed())
		})

		It("A user disposes of the gate it filed itself", func(ctx SpecContext) {
			filed := gate(childOf("n14", node1, user.ID))
			Expect(bundle.Authorize(ctx, nodeOp(user, "complete_node", &filed, ptr(withStatus(filed, "done")), nil))).To(Succeed())
			Expect(bundle.Authorize(ctx, nodeOp(user, "drop_node", &filed, ptr(withStatus(filed, "dropped")), nil))).To(Succeed())
			reclassified := filed
			reclassified.AssigneeKind = ptr("agent")
			Expect(bundle.Authorize(ctx, nodeOp(user, "update_node", &filed, &reclassified, nil))).To(Succeed())
			Expect(bundle.Authorize(ctx, removeEdge(user, filed, node7, nil))).To(Succeed())
		})

		It("Every veto over a gate names the gate and the next move", func(ctx SpecContext) {
			leaseB := &policy.Lease{ID: "L9", NodeID: "n9", Holder: "B"}
			reclassified := node9
			reclassified.AssigneeKind = ptr("agent")
			for _, c := range []struct {
				in     policy.Input
				reason string
			}{
				{nodeOp(agentB, "complete_node", &node9, done, leaseB),
					"only a user may complete gate n9: a person completes it; comment your recommendation on it and release your lease with reason gate"},
				{nodeOp(agentA, "complete_node", &node9, done, leaseA),
					"the principal that filed gate n9 may not complete it: a person decides it; comment what you found on it and release your lease with reason gate"},
				{nodeOp(agentA, "drop_node", &node9, ptr(withStatus(node9, "dropped")), leaseA),
					"the principal that filed gate n9 may not drop it: a person decides it; comment what you found on it and release your lease with reason gate"},
				{nodeOp(agentA, "update_node", &node9, &reclassified, leaseA),
					"the principal that filed gate n9 may not change its assignee_kind: that would take the gate away from the person it asks; comment what you need on it and release your lease with reason gate"},
				{removeEdge(agentA, node9, node7, leaseA),
					"the principal that filed gate n9 may not unlink it: your work waits on it until a person completes it; comment what you need on it and release your lease with reason gate"},
				{claim(agentB, node9), "an agent may not claim gate n9: a person performs it; do the preparation in a node of your own that the gate depends on, and leave the gate ready for a person"},
			} {
				err := bundle.Authorize(ctx, c.in)
				Expect(err).To(BeDenied(), c.reason)
				var denied *policy.DeniedError
				Expect(errors.As(err, &denied)).To(BeTrue())
				Expect(denied.Reasons).To(ContainElement(c.reason))
			}
		})
	})

	Describe("Failing closed", func() {
		It("denies when the bundle has no modules", func(ctx SpecContext) {
			empty, err := policy.Load(ctx, fstest.MapFS{"README.md": {Data: []byte("nothing here")}})
			Expect(err).To(MatchError(ContainSubstring("no modules")))
			Expect(empty.Authorize(ctx, nodeOp(user, "update_node", &node7, &node7, nil))).To(BeDenied())
		})

		It("denies when evaluation errors rather than answers", func(ctx SpecContext) {
			erroring, err := policy.Load(ctx, fstest.MapFS{"skift.rego": {Data: []byte(
				"package skift.authz\n\ndecision := {\"allow\": true, \"reasons\": set()} if {\n\t1 / count(input.nothing) == 0\n}\n",
			)}})
			Expect(err).NotTo(HaveOccurred())
			denied := erroring.Authorize(ctx, nodeOp(user, "update_node", &node7, &node7, nil))
			Expect(denied).To(BeDenied())
			Expect(denied).To(MatchError(ContainSubstring("policy evaluation failed")))
		})

		It("denies when the decision is undefined or malformed", func(ctx SpecContext) {
			for name, src := range map[string]string{
				"undefined":         "package skift.authz\n\nallow := true\n",
				"not an object":     "package skift.authz\n\ndecision := true\n",
				"allow not boolean": "package skift.authz\n\ndecision := {\"allow\": \"yes\"}\n",
			} {
				b, err := policy.Load(ctx, fstest.MapFS{"skift.rego": {Data: []byte(src)}})
				Expect(err).NotTo(HaveOccurred(), name)
				Expect(b.Authorize(ctx, nodeOp(user, "update_node", &node7, &node7, nil))).To(BeDenied(), name)
			}
		})

		It("denies an action no rule names, and says so", func(ctx SpecContext) {
			err := bundle.Authorize(ctx, policy.Input{Principal: user, Action: "annihilate_node"})
			Expect(err).To(BeDenied())
			var denied *policy.DeniedError
			Expect(errors.As(err, &denied)).To(BeTrue())
			Expect(denied.Action).To(Equal("annihilate_node"))
			Expect(denied.Reasons).To(ConsistOf("no rule permits user annihilate_node"))
		})
	})

	Describe("Bundle version", func() {
		It("is the digest of the modules, so the same policy has the same version", func(ctx SpecContext) {
			again, err := policy.LoadDefault(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(bundle.Version()).To(MatchRegexp(`^sha256:[0-9a-f]{64}$`))
			Expect(again.Version()).To(Equal(bundle.Version()))

			other, err := policy.Load(ctx, fstest.MapFS{"skift.rego": {Data: []byte("package skift.authz\n\ndecision := {\"allow\": false, \"reasons\": set()}\n")}})
			Expect(err).NotTo(HaveOccurred())
			Expect(other.Version()).NotTo(Equal(bundle.Version()))
			Expect((*policy.Bundle)(nil).Version()).To(BeEmpty())
		})

		It("leaves the opa test suite out of the bundle", func(ctx SpecContext) {
			b, err := policy.Load(ctx, fstest.MapFS{
				"skift.rego":      {Data: []byte("package skift.authz\n\ndecision := {\"allow\": true, \"reasons\": set()}\n")},
				"skift_test.rego": {Data: []byte("this is not rego\n")},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(b.Authorize(ctx, policy.Input{Principal: user, Action: "anything"})).To(Succeed())
		})
	})
})

func ptr[T any](v T) *T { return &v }
