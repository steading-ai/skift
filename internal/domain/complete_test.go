package domain_test

import (
	"context"
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// complete is a complete_node operation carrying a body and artifacts.
func complete(node, body string, artifacts ...domain.Artifact) domain.Op {
	return domain.Op{CompleteNode: &domain.CompleteNode{Node: node, Result: domain.NodeResult{Body: body, Artifacts: artifacts}}}
}

// textArtifact is an artifact of one inline part.
func textArtifact(name, text string) domain.Artifact {
	return domain.Artifact{Name: name, Parts: []domain.Part{{Text: &domain.TextPart{Text: text, MediaType: "text/markdown"}}}}
}

const specDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// inputs fetches the inputs of a node in a transaction of its own.
func (g *graph) inputs(ctx context.Context, ref string) []domain.Input {
	GinkgoHelper()
	var in []domain.Input
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		in, err = domain.Inputs(ctx, tx, g.org, g.id(ref))
		return err
	})).To(Succeed())
	return in
}

func (g *graph) results(ctx context.Context, ref string) []store.Result {
	GinkgoHelper()
	rs, err := g.s.Queries().ListResultsForNode(ctx, store.ListResultsForNodeParams{OrgID: g.org, NodeID: g.id(ref)})
	Expect(err).NotTo(HaveOccurred())
	return rs
}

func (g *graph) statuses(ctx context.Context, refs ...string) []string {
	GinkgoHelper()
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, g.getNode(ctx, r).Status)
	}
	return out
}

var _ = Describe("Completion, results, drop, and reopen", Label(testsupport.DBLabel), func() {
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
		It("Downstream context comes from the graph", func(ctx SpecContext) {
			By("given node 3 blocked by nodes 1 and 2, both done with results")
			g.mustApply(ctx, actor, "setup",
				create("one", "One"), create("two", "Two"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "three", Title: "Three", BlockedBy: []string{"one", "two"}}},
			)
			alice, bob := g.agent(ctx), g.agent(ctx)
			first := g.mustClaim(ctx, alice, claimReq("claim-1", time.Minute))
			Expect(first.Node.Number).To(Equal(int64(1)))
			second := g.mustClaim(ctx, bob, claimReq("claim-2", time.Minute))
			Expect(second.Node.Number).To(Equal(int64(2)))
			_, err := g.applyWithLease(ctx, alice, "done-1", first.Lease.ID,
				complete("#1", "# One\n\nShipped the schema.", textArtifact("schema", "CREATE TABLE t ()")))
			Expect(err).NotTo(HaveOccurred())
			_, err = g.applyWithLease(ctx, bob, "done-2", second.Lease.ID,
				complete("#2", "# Two\n\nOpened the PR.", domain.Artifact{
					Name: "pr", Description: "the change", Metadata: map[string]any{"repo": "acme/platform"},
					Parts: []domain.Part{{Reference: &domain.ReferencePart{URI: "git+https://example.com/acme/platform/pull/7", Digest: specDigest, MediaType: "text/x-diff", Size: 1234}}},
				}))
			Expect(err).NotTo(HaveOccurred())

			By("when I fetch the inputs for node 3")
			in := g.inputs(ctx, "#3")

			By("then both results and their artifacts are returned with their authors")
			Expect(in).To(HaveLen(2))
			Expect(in[0].Node.Number).To(Equal(int64(1)))
			Expect(in[0].Result).NotTo(BeNil())
			Expect(in[0].Result.Body).To(Equal("# One\n\nShipped the schema."))
			Expect(in[0].Result.CompletedBy).To(Equal(alice.PrincipalID))
			Expect(in[0].Artifacts).To(HaveLen(1))
			Expect(in[0].Artifacts[0].Name).To(Equal("schema"))
			Expect(in[0].Artifacts[0].CreatedBy).To(Equal(alice.PrincipalID))
			Expect(in[0].Artifacts[0].Parts).To(HaveLen(1))
			Expect(in[0].Artifacts[0].Parts[0].Kind).To(Equal(domain.PartText))
			Expect(in[0].Artifacts[0].Parts[0].Text).To(HaveValue(Equal("CREATE TABLE t ()")))
			Expect(in[0].Artifacts[0].Parts[0].MediaType).To(HaveValue(Equal("text/markdown")))
			Expect(in[0].Artifacts[0].Parts[0].Size).To(HaveValue(Equal(int64(len("CREATE TABLE t ()")))))

			Expect(in[1].Node.Number).To(Equal(int64(2)))
			Expect(in[1].Result.Body).To(Equal("# Two\n\nOpened the PR."))
			Expect(in[1].Result.CompletedBy).To(Equal(bob.PrincipalID))
			Expect(in[1].Artifacts).To(HaveLen(1))
			Expect(in[1].Artifacts[0].Name).To(Equal("pr"))
			Expect(in[1].Artifacts[0].Description).To(Equal("the change"))
			Expect(in[1].Artifacts[0].CreatedBy).To(Equal(bob.PrincipalID))
			Expect(in[1].Artifacts[0].Metadata).To(MatchJSON(`{"repo": "acme/platform"}`))
			part := in[1].Artifacts[0].Parts[0]
			Expect(part.Kind).To(Equal(domain.PartReference))
			Expect(part.ReferenceUri).To(HaveValue(Equal("git+https://example.com/acme/platform/pull/7")))
			Expect(part.ReferenceDigest).To(HaveValue(Equal(specDigest)))
			Expect(part.MediaType).To(HaveValue(Equal("text/x-diff")))
			Expect(part.Size).To(HaveValue(Equal(int64(1234))))
		})

		It("Axing functionality takes the subtree", func(ctx SpecContext) {
			By("given node 1 blocks nodes 2 and 3, and node 3 blocks node 4")
			g.mustApply(ctx, actor, "setup",
				create("one", "One"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "two", Title: "Two", BlockedBy: []string{"one"}}},
				domain.Op{CreateNode: &domain.CreateNode{Alias: "three", Title: "Three", BlockedBy: []string{"one"}}},
				domain.Op{CreateNode: &domain.CreateNode{Alias: "four", Title: "Four", BlockedBy: []string{"three"}}},
			)

			By("when node 1 is dropped with cascade")
			r := g.mustApply(ctx, actor, "axe", domain.Op{DropNode: &domain.DropNode{
				Node: "#1", Reason: domain.DropNoLongerRelevant, Cascade: true,
			}})

			By("then nodes 1 through 4 are dropped in one transaction")
			Expect(g.statuses(ctx, "#1", "#2", "#3", "#4")).To(Equal([]string{"dropped", "dropped", "dropped", "dropped"}))
			Expect(r.Nodes).To(HaveLen(4))
			Expect(ops(r.Events)).To(Equal([]string{domain.OpDropNode, domain.OpDropNode, domain.OpDropNode, domain.OpDropNode}))
			Expect(ops(g.eventsFor(ctx, "axe"))).To(HaveLen(4), "every drop is recorded under the one request")
			for _, e := range r.Events {
				Expect(e.Reason).To(HaveValue(Equal(domain.DropNoLongerRelevant)))
			}
		})

		It("Reopening does not un-complete descendants", func(ctx SpecContext) {
			By("given node 1 is done and node 2, which it blocked, is also done")
			g.mustApply(ctx, actor, "setup",
				create("one", "One"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "two", Title: "Two", BlockedBy: []string{"one"}}},
				complete("one", "first pass"),
				complete("two", "built on the first pass"),
			)
			Expect(g.statuses(ctx, "#1", "#2")).To(Equal([]string{"done", "done"}))

			By("when node 1 is reopened")
			r := g.mustApply(ctx, actor, "reopen", reopen("#1", "review found a gap"))

			By("then node 2 remains done")
			Expect(g.getNode(ctx, "#1").Status).To(Equal("open"))
			Expect(g.getNode(ctx, "#2").Status).To(Equal("done"))
			Expect(r.Nodes).To(HaveLen(1))
			Expect(ops(r.Events)).To(Equal([]string{domain.OpReopenNode}))
			Expect(g.results(ctx, "#2")).To(HaveLen(1), "node 2 keeps its result")
			Expect(g.nodeEvents(ctx, "#2")).To(HaveLen(3), "create, add_edge, complete, and nothing from the reopen")
		})
	})

	Describe("Completing", func() {
		It("requires a live lease matching the fencing token on a held node", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))
			agentA, agentB := g.agent(ctx), g.agent(ctx)
			first := g.mustClaim(ctx, agentA, claimReq("a-claim", time.Minute))

			By("a completion carrying no lease is rejected")
			_, err := g.apply(ctx, actor, "no-lease", complete("#1", "done"))
			Expect(err).To(MatchError(domain.ErrLeaseRequired))

			By("a completion carrying a reclaimed lease is rejected")
			g.age(ctx, first.Lease.ID, 2*time.Minute, -time.Minute)
			second := g.mustClaim(ctx, agentB, claimReq("b-claim", time.Minute))
			Expect(second.Node.ID).To(Equal(first.Node.ID))
			_, err = g.applyWithLease(ctx, agentA, "stale", first.Lease.ID, complete("#1", "late"))
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))
			Expect(g.getNode(ctx, "#1").Status).To(Equal("open"))
			Expect(g.results(ctx, "#1")).To(BeEmpty())

			By("a completion carrying the node's current lease succeeds")
			r, err := g.applyWithLease(ctx, agentB, "current", second.Lease.ID, complete("#1", "finished"))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Nodes[0].Status).To(Equal("done"))
			Expect(ops(r.Events)).To(Equal([]string{domain.OpCompleteNode}))
			Expect(g.results(ctx, "#1")).To(HaveLen(1))
			Expect(g.results(ctx, "#1")[0].CompletedBy).To(Equal(agentB.PrincipalID))
		})

		It("denies an agent completing a node it holds no lease on", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))
			_, err := g.apply(ctx, g.agent(ctx), "no-claim", complete("#1", "done"))
			var denial *domain.Denial
			Expect(errors.As(err, &denial)).To(BeTrue(), "expected a policy denial, got %v", err)
			Expect(denial.Op).To(Equal(domain.OpCompleteNode))
			Expect(g.getNode(ctx, "#1").Status).To(Equal("open"))
		})

		It("records the result and artifacts on the event, and satisfies what the node blocked", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup",
				create("a", "A"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "b", Title: "B", BlockedBy: []string{"a"}}},
			)
			Expect(g.explain(ctx, "#2").Ready()).To(BeFalse())

			r := g.mustApply(ctx, actor, "done", complete("#1", "the result", textArtifact("notes", "some notes")))

			Expect(r.Events).To(HaveLen(1))
			Expect(field(r.Events[0].Before, "Status")).To(Equal("open"))
			after := field(r.Events[0].After, "node").(map[string]any)
			Expect(after["Status"]).To(Equal("done"))
			Expect(field(r.Events[0].After, "result").(map[string]any)["Body"]).To(Equal("the result"))
			Expect(field(r.Events[0].After, "artifacts")).To(HaveLen(1))
			Expect(g.explain(ctx, "#2").Ready()).To(BeTrue())
		})

		It("keeps every result across reopen, with the newest current", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup",
				create("a", "A"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "b", Title: "B", BlockedBy: []string{"a"}}},
				complete("a", "first"),
				reopen("a", "not quite"),
				complete("a", "second"),
			)
			Expect(g.results(ctx, "#1")).To(HaveLen(2))
			in := g.inputs(ctx, "#2")
			Expect(in).To(HaveLen(1))
			Expect(in[0].Result.Body).To(Equal("second"))
		})

		It("rejects a done or dropped node, a missing body, and a body over the cap before writing", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"), drop("b", domain.DropSuperseded))

			_, err := g.apply(ctx, actor, "empty", complete("#1", ""))
			Expect(err).To(MatchError(domain.ErrResultRequired))
			var opErr *domain.OpError
			Expect(errors.As(err, &opErr)).To(BeTrue(), "expected an operation error, got %v", err)
			Expect(opErr.Op).To(Equal(domain.OpCompleteNode))
			Expect(g.countEvents(ctx, "empty")).To(BeZero())

			_, err = g.apply(ctx, actor, "huge", complete("#1", strings.Repeat("x", domain.MaxResultBytes+1)))
			Expect(err).To(MatchError(domain.ErrResultTooLarge))

			_, err = g.apply(ctx, actor, "dropped", complete("#2", "done"))
			Expect(err).To(MatchError(ContainSubstring("is dropped")))

			g.mustApply(ctx, actor, "done", complete("#1", "done"))
			_, err = g.apply(ctx, actor, "again", complete("#1", "done twice"))
			Expect(err).To(MatchError(ContainSubstring("is done")))
			Expect(g.results(ctx, "#1")).To(HaveLen(1))
		})

		It("rejects malformed artifacts before writing", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))
			reject := func(requestID string, art domain.Artifact, want string) {
				GinkgoHelper()
				_, err := g.apply(ctx, actor, requestID, complete("#1", "done", art))
				Expect(err).To(MatchError(ContainSubstring(want)))
			}
			text := &domain.TextPart{Text: "t"}
			reject("unnamed", domain.Artifact{Parts: []domain.Part{{Text: text}}}, "a name is required")
			reject("partless", domain.Artifact{Name: "x"}, "at least one part")
			reject("empty-part", domain.Artifact{Name: "x", Parts: []domain.Part{{}}}, "exactly one of")
			reject("two-kinds", domain.Artifact{Name: "x", Parts: []domain.Part{{Text: text, Blob: &domain.BlobPart{Digest: specDigest}}}}, "exactly one of")
			reject("bad-digest", domain.Artifact{Name: "x", Parts: []domain.Part{{Reference: &domain.ReferencePart{URI: "https://x", Digest: "md5:abc"}}}}, "not sha256")
			reject("no-uri", domain.Artifact{Name: "x", Parts: []domain.Part{{Reference: &domain.ReferencePart{Digest: specDigest}}}}, "needs a uri")
			reject("big", domain.Artifact{Name: "x", Parts: []domain.Part{{Text: &domain.TextPart{Text: strings.Repeat("x", domain.MaxInlinePartBytes+1)}}}}, domain.ErrPartTooLarge.Error())

			_, err := g.apply(ctx, actor, "twice", complete("#1", "done", textArtifact("x", "1"), textArtifact("x", "2")))
			Expect(err).To(MatchError(ContainSubstring("named twice")))

			_, err = g.apply(ctx, actor, "no-blob", complete("#1", "done", domain.Artifact{Name: "x", Parts: []domain.Part{{Blob: &domain.BlobPart{Digest: specDigest}}}}))
			Expect(err).To(MatchError(domain.ErrBlobNotFound))
			Expect(g.getNode(ctx, "#1").Status).To(Equal("open"))
			Expect(g.results(ctx, "#1")).To(BeEmpty())
		})

		It("attaches a managed blob the organization holds, with its media type and size", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				_, err := tx.CreateBlob(ctx, store.CreateBlobParams{OrgID: g.org, Digest: specDigest, MediaType: "application/zip", Size: 99, Backend: "fs"})
				Expect(err).NotTo(HaveOccurred())
			})
			g.mustApply(ctx, actor, "done", complete("#1", "done", domain.Artifact{Name: "bundle", Parts: []domain.Part{{Blob: &domain.BlobPart{Digest: specDigest}}}}))
			g.mustApply(ctx, actor, "next", domain.Op{CreateNode: &domain.CreateNode{Alias: "b", Title: "B", BlockedBy: []string{"#1"}}})

			in := g.inputs(ctx, "#2")
			part := in[0].Artifacts[0].Parts[0]
			Expect(part.Kind).To(Equal(domain.PartBlob))
			Expect(part.BlobDigest).To(HaveValue(Equal(specDigest)))
			Expect(part.MediaType).To(HaveValue(Equal("application/zip")))
			Expect(part.Size).To(HaveValue(Equal(int64(99))))
		})
	})

	Describe("Dropping", func() {
		It("without cascade leaves dependents open and unblocked", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup",
				create("one", "One"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "two", Title: "Two", BlockedBy: []string{"one"}}},
				domain.Op{CreateNode: &domain.CreateNode{Alias: "three", Title: "Three", BlockedBy: []string{"two"}}},
			)
			Expect(g.explain(ctx, "#2").Ready()).To(BeFalse())

			r := g.mustApply(ctx, actor, "drop", drop("#1", domain.DropNotViable))

			Expect(g.statuses(ctx, "#1", "#2", "#3")).To(Equal([]string{"dropped", "open", "open"}))
			Expect(r.Nodes).To(HaveLen(1))
			Expect(g.explain(ctx, "#2").Ready()).To(BeTrue())
			Expect(g.explain(ctx, "#3").Ready()).To(BeFalse(), "node 3 still waits on node 2")
		})

		It("with cascade rolls the whole subtree back when one node in it cannot be dropped", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup",
				create("one", "One"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "two", Title: "Two", BlockedBy: []string{"one"}}},
				domain.Op{CreateNode: &domain.CreateNode{Alias: "three", Title: "Three", Priority: "p0"}},
			)
			agent := g.agent(ctx)
			holder := g.mustClaim(ctx, agent, claimReq("claim", time.Minute))
			Expect(holder.Node.Number).To(Equal(int64(3)))
			_, err := g.applyWithLease(ctx, agent, "link", holder.Lease.ID, addEdge("#2", "#3"))
			Expect(err).NotTo(HaveOccurred())

			_, err = g.apply(ctx, actor, "axe", domain.Op{DropNode: &domain.DropNode{Node: "#1", Reason: domain.DropSuperseded, Cascade: true}})

			Expect(err).To(MatchError(domain.ErrLeaseRequired), "node 3 is held, and the patch carries no lease")
			Expect(g.statuses(ctx, "#1", "#2", "#3")).To(Equal([]string{"open", "open", "open"}))
			Expect(g.countEvents(ctx, "axe")).To(BeZero())
		})
	})

	Describe("Inputs", func() {
		It("returns a predecessor that has not completed with no result", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup",
				create("open", "Open"), create("dropped", "Dropped"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "c", Title: "C", BlockedBy: []string{"open", "dropped"}}},
				create("unrelated", "Unrelated"),
				drop("dropped", domain.DropSuperseded),
			)
			in := g.inputs(ctx, "#3")
			Expect(in).To(HaveLen(2))
			Expect(in[0].Node.Status).To(Equal("open"))
			Expect(in[0].Result).To(BeNil())
			Expect(in[1].Node.Status).To(Equal("dropped"))
			Expect(in[1].Result).To(BeNil())
			Expect(in[1].Artifacts).To(BeEmpty())
		})

		It("reports an unknown node", func(ctx SpecContext) {
			err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := domain.Inputs(ctx, tx, g.org, domain.NewULID())
				return err
			})
			Expect(err).To(MatchError(domain.ErrNodeNotFound))
		})
	})
})
