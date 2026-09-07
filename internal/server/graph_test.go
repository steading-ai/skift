package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/server"
	"github.com/ivy/dagger/internal/server/interceptor"
	"github.com/ivy/dagger/internal/testsupport"
)

var _ = Describe("GraphService", Label(testsupport.DBLabel), func() {
	var f *fixture

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx)
	})

	Describe("ApplyPatch", func() {
		It("A rejected patch reports why", func(ctx SpecContext) {
			By("given a patch forming a cycle")
			project := f.project(ctx, "Platform")
			patch := []*daggerv1.PatchOp{
				createOp("a", "First"), createOp("b", "Second"),
				edgeOp("a", "b"), edgeOp("b", "a"),
			}

			By("when it is applied over the wire")
			_, err := f.apply(ctx, project.GetSlug(), patch...)

			By("then the response is a failed precondition naming the cycle path")
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(SatisfyAll(
				ContainSubstring("cycle"),
				ContainSubstring("platform#1"),
				ContainSubstring("platform#2"),
			))

			By("and nothing of the patch was kept")
			_, err = f.graph().GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{Project: "platform", Node: "#1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})

		It("A stale lease is reported distinctly", func(ctx SpecContext) {
			By("given a write with an expired lease id")
			project := f.project(ctx, "Platform")
			f.mustApply(ctx, createOp("a", "Work"))
			claimed := f.claim(ctx, project.GetSlug(), durationpb.New(time.Millisecond))
			Expect(claimed.GetLease().GetId()).NotTo(BeEmpty())
			time.Sleep(50 * time.Millisecond)

			f.as(f.agent)
			_, stale := f.applyWith(ctx, &daggerv1.ApplyPatchRequest{
				RequestId: newID(), Project: "platform", LeaseId: claimed.GetLease().GetId(),
				Ops: []*daggerv1.PatchOp{updateOp(&daggerv1.UpdateNodeOp{Node: "#1", Title: ptr("Renamed")})},
			})

			By("and a write the policy refuses, for comparison")
			_, denied := f.apply(ctx, "platform", createOpWith(&daggerv1.CreateNodeOp{
				Alias: "x", Parent: "#1", Title: "An agent may not file under another principal's node without a lease",
			}))

			By("then the response code distinguishes it from a permission denial")
			Expect(connect.CodeOf(stale)).To(Equal(connect.CodeFailedPrecondition))
			Expect(stale.Error()).To(ContainSubstring("lease is not live"))
			Expect(connect.CodeOf(denied)).To(Equal(connect.CodePermissionDenied))
			Expect(connect.CodeOf(stale)).NotTo(Equal(connect.CodeOf(denied)))
		})

		It("Output-only fields set by a client are ignored, never honored", func(ctx SpecContext) {
			By("given an artifact whose text part claims a digest and size the server did not compute")
			f.project(ctx, "Platform")
			forged := &daggerv1.Artifact{Name: "notes", Parts: []*daggerv1.Part{{Content: &daggerv1.Part_Text{Text: &daggerv1.TextPart{
				Text: "hello", Digest: "sha256:forged", Size: 999,
			}}}}}

			By("when it is attached over the wire")
			f.mustApply(ctx, createOp("a", "Work"), attachOp("a", forged))

			By("then the stored part carries the server's digest and size")
			sum := sha256.Sum256([]byte("hello"))
			part := f.getNode(ctx, "platform", "#1").GetArtifacts()[0].GetParts()[0].GetText()
			Expect(part.GetDigest()).To(Equal("sha256:" + hex.EncodeToString(sum[:])))
			Expect(part.GetSize()).To(BeEquivalentTo(5))
		})

		It("returns the nodes, aliases, and events a patch produced, attributed to the caller", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			res, err := f.applyWith(ctx, &daggerv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", View: daggerv1.View_VIEW_FULL, Ops: []*daggerv1.PatchOp{
				createOp("root", "Root"),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "child", Title: "Child", Parent: "root", Priority: daggerv1.Priority_PRIORITY_P0, Labels: []string{"infra"}}),
			}})
			Expect(err).NotTo(HaveOccurred())

			Expect(res.GetAliases()).To(Equal(map[string]string{"root": "platform#1", "child": "platform#2"}))
			Expect(numbers(res.GetNodes())).To(Equal([]int64{1, 2}))
			root, child := res.GetNodes()[0], res.GetNodes()[1]
			Expect(root.GetStatus()).To(Equal(daggerv1.Status_STATUS_OPEN))
			Expect(root.GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P2), "the default priority")
			Expect(root.GetReady()).To(BeFalse(), "an open child keeps the parent from being ready")
			Expect(root.GetHumanTouched()).To(BeTrue(), "a user created it")
			Expect(child.GetParentId()).To(Equal(root.GetId()))
			Expect(child.GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P0))
			Expect(child.GetLabels()).To(Equal([]string{"infra"}))
			Expect(child.GetReady()).To(BeTrue())
			Expect(child.GetContextId()).NotTo(BeEmpty())
			Expect(child.GetReference()).To(Equal("platform#2"))

			Expect(res.GetEvents()).To(HaveLen(2))
			Expect(res.GetEventCount()).To(BeEquivalentTo(2))
			ev := res.GetEvents()[0]
			Expect(ev.GetOp()).To(Equal("create_node"))
			Expect(ev.GetPrincipal()).To(Equal(f.user.ID))
			Expect(ev.GetPrincipalKind()).To(Equal(daggerv1.PrincipalKind_PRINCIPAL_KIND_USER))
			Expect(ev.GetIdentityScheme()).To(Equal(interceptor.SchemeUnix))
			Expect(ev.GetAfter().GetFields()).To(HaveKey("Title"))
			Expect(ev.GetBefore()).To(BeNil())
		})

		It("replays a repeated request id and refuses the same id with different content", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			req := &daggerv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", Ops: []*daggerv1.PatchOp{createOp("a", "Once")}}
			first, err := f.applyWith(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			again, err := f.applyWith(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.GetNodes()[0].GetId()).To(Equal(first.GetNodes()[0].GetId()))

			req.Ops = []*daggerv1.PatchOp{createOp("a", "Different")}
			_, err = f.applyWith(ctx, req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeAlreadyExists))
		})

		It("rejects a patch over a ceiling as resource exhausted and a malformed one as invalid", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			_, err := f.apply(ctx, "platform", createOpWith(&daggerv1.CreateNodeOp{Title: "Big", Body: string(make([]byte, 257<<10))}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeResourceExhausted))

			_, err = f.applyWith(ctx, &daggerv1.ApplyPatchRequest{Project: "platform", Ops: []*daggerv1.PatchOp{createOp("a", "No request id")}})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

			_, err = f.apply(ctx, "platform", &daggerv1.PatchOp{Op: &daggerv1.PatchOp_DropNode{DropNode: &daggerv1.DropNodeOp{Node: "#1"}}})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument), "a drop needs a reason")

			_, err = f.apply(ctx, "nowhere", createOp("a", "Lost"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})
	})

	Describe("views", func() {
		// bigPatch is a patch of one create_node per alias, each with a
		// body of size bytes.
		bigPatch := func(aliases []string, size int) []*daggerv1.PatchOp {
			ops := make([]*daggerv1.PatchOp, 0, len(aliases))
			for _, alias := range aliases {
				ops = append(ops, createOpWith(&daggerv1.CreateNodeOp{Alias: alias, Title: "Node " + alias, Body: strings.Repeat(alias[:1], size)}))
			}
			return ops
		}

		Describe("Scenario: A write does not echo what it was sent", func() {
			It("A write does not echo what it was sent", func(ctx SpecContext) {
				By("given a patch of one create_node with a 3000-byte body")
				f.project(ctx, "Platform")
				patch := bigPatch([]string{"big"}, 3000)

				By("when it is applied with no view")
				res := f.mustApply(ctx, patch...)

				By("then the response maps the alias to a reference")
				Expect(res.GetAliases()).To(Equal(map[string]string{"big": "platform#1"}))
				Expect(res.GetNodes()).To(HaveLen(1))
				Expect(res.GetNodes()[0].GetReference()).To(Equal("platform#1"))
				Expect(res.GetNodes()[0].GetNumber()).To(BeEquivalentTo(1))
				Expect(res.GetNodes()[0].GetReady()).To(BeTrue())

				By("and the response contains no body and no events")
				Expect(res.GetNodes()[0].GetBody()).To(BeEmpty())
				Expect(res.GetEvents()).To(BeEmpty())
				wire := wireJSON(res)
				Expect(wire).NotTo(ContainSubstring(`"body"`))
				Expect(wire).NotTo(ContainSubstring(`"events"`))
				Expect(wire).NotTo(ContainSubstring(`"title"`), "refs is not a summary")

				By("and the response carries event_count 1")
				Expect(res.GetEventCount()).To(BeEquivalentTo(1))
				Expect(res.GetRequestId()).To(HaveLen(26))
			})
		})

		Describe("Scenario: Full is available on request", func() {
			It("Full is available on request", func(ctx SpecContext) {
				By("given the same patch")
				f.project(ctx, "Platform")
				patch := bigPatch([]string{"big"}, 3000)

				By("when it is applied with view full")
				res, err := f.applyWith(ctx, &daggerv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", Ops: patch, View: daggerv1.View_VIEW_FULL})
				Expect(err).NotTo(HaveOccurred())

				By("then the response contains the events and the node bodies")
				Expect(res.GetEvents()).To(HaveLen(1))
				Expect(res.GetEvents()[0].GetOp()).To(Equal("create_node"))
				Expect(res.GetNodes()).To(HaveLen(1))
				Expect(res.GetNodes()[0].GetBody()).To(Equal(strings.Repeat("b", 3000)))
				Expect(res.GetNodes()[0].GetTitle()).To(Equal("Node big"))
				Expect(res.GetEventCount()).To(BeEquivalentTo(1))
			})
		})

		It("keeps a 28-operation patch of 2 KB bodies under 4 KB on the wire", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			aliases := make([]string, 0, 28)
			for i := range 28 {
				aliases = append(aliases, fmt.Sprintf("n%d", i+1))
			}
			res := f.mustApply(ctx, bigPatch(aliases, 2048)...)

			Expect(res.GetNodes()).To(HaveLen(28))
			Expect(res.GetAliases()).To(HaveLen(28))
			Expect(res.GetEventCount()).To(BeEquivalentTo(28))
			wire := wireJSON(res)
			Expect(wire).NotTo(ContainSubstring(`"body"`))
			Expect(wire).NotTo(ContainSubstring(`"events"`))
			Expect(len(wire)).To(BeNumerically("<", 4096), "the response was %d bytes", len(wire))
		})

		It("projects a summary as the fields that choose work, and never the body", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			f.mustApply(ctx, createOpWith(&daggerv1.CreateNodeOp{Alias: "a", Title: "Choose me", Kind: "bug", Body: "the body", Priority: daggerv1.Priority_PRIORITY_P1, Labels: []string{"infra"}}))

			list, err := f.graph().ListNodes(ctx, connect.NewRequest(&daggerv1.ListNodesRequest{Project: "platform"}))
			Expect(err).NotTo(HaveOccurred())
			n := list.Msg.GetNodes()[0]
			Expect(n.GetTitle()).To(Equal("Choose me"))
			Expect(n.GetKind()).To(Equal("bug"))
			Expect(n.GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P1))
			Expect(n.GetStatus()).To(Equal(daggerv1.Status_STATUS_OPEN))
			Expect(n.GetLabels()).To(Equal([]string{"infra"}))
			Expect(n.GetReference()).To(Equal("platform#1"))
			Expect(n.GetReady()).To(BeTrue())
			Expect(wireJSON(list.Msg)).NotTo(ContainSubstring(`"body"`))

			hits, err := f.graph().Search(ctx, connect.NewRequest(&daggerv1.SearchRequest{Query: "choose"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(hits.Msg.GetHits()).To(HaveLen(1))
			Expect(hits.Msg.GetHits()[0].GetNode().GetReference()).To(Equal("platform#1"))
			Expect(hits.Msg.GetHits()[0].GetNode().GetTitle()).To(Equal("Choose me"))
			Expect(wireJSON(hits.Msg)).NotTo(ContainSubstring(`"body"`))

			full, err := f.graph().Search(ctx, connect.NewRequest(&daggerv1.SearchRequest{Query: "choose", View: daggerv1.View_VIEW_FULL}))
			Expect(err).NotTo(HaveOccurred())
			Expect(full.Msg.GetHits()[0].GetNode().GetBody()).To(Equal("the body"))
		})
	})

	Describe("GetNode", func() {
		It("returns the body with no view, and leaves content out of a narrower one", func(ctx SpecContext) {
			project := f.project(ctx, "Platform")
			f.mustApply(ctx, createOpWith(&daggerv1.CreateNodeOp{Alias: "a", Title: "Read me", Body: "the prompt"}), commentOp("a", "a note"))

			shown := f.getNode(ctx, "platform", "#1")
			Expect(shown.GetNode().GetBody()).To(Equal("the prompt"))
			Expect(shown.GetNode().GetReference()).To(Equal("platform#1"))
			Expect(shown.GetComments()).To(HaveLen(1))

			res, err := f.graph().GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{Project: "platform", Node: "#1", View: daggerv1.View_VIEW_SUMMARY}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetNode().GetTitle()).To(Equal("Read me"))
			Expect(res.Msg.GetNode().GetProjectId()).To(Equal(project.GetId()), "a summary says which project the node is in; search crosses them")
			Expect(res.Msg.GetNode().GetBody()).To(BeEmpty())
			Expect(res.Msg.GetComments()).To(BeEmpty())
		})

		It("returns a node with its edges, result, artifacts, comments, and lease", func(ctx SpecContext) {
			project := f.project(ctx, "Platform")
			f.mustApply(ctx, createOp("a", "First"), createOp("b", "Second"), edgeOp("a", "b"), commentOp("b", "Looks right"))
			f.mustApply(ctx, completeOp("#1", "Done, see the notes", textArtifact("notes", "the notes")))

			second := f.getNode(ctx, "platform", "#2")
			Expect(second.GetNode().GetTitle()).To(Equal("Second"))
			Expect(second.GetEdges()).To(HaveLen(1))
			Expect(second.GetEdges()[0].GetKind()).To(Equal(daggerv1.EdgeKind_EDGE_KIND_BLOCKS))
			Expect(second.GetComments()).To(HaveLen(1))
			Expect(second.GetComments()[0].GetBody()).To(Equal("Looks right"))
			Expect(second.GetComments()[0].GetAuthor()).To(Equal(f.user.ID))
			Expect(second.GetNode().GetReady()).To(BeTrue(), "its blocker is done")

			first := f.getNode(ctx, "", "platform#1")
			Expect(first.GetNode().GetStatus()).To(Equal(daggerv1.Status_STATUS_DONE))
			Expect(first.GetResult().GetBody()).To(Equal("Done, see the notes"))
			Expect(first.GetResult().GetArtifacts()).To(HaveLen(1))
			Expect(first.GetArtifacts()[0].GetName()).To(Equal("notes"))
			Expect(first.GetLease()).To(BeNil())

			claimed := f.claim(ctx, project.GetSlug(), nil)
			Expect(claimed.GetNode().GetNumber()).To(BeEquivalentTo(2))
			held := f.getNode(ctx, "platform", "#2")
			Expect(held.GetLease().GetId()).To(Equal(claimed.GetLease().GetId()))
			Expect(held.GetLease().GetHolder()).To(Equal(f.agent.ID))
			Expect(held.GetNode().GetClaimed()).To(BeTrue())
		})

		It("reports a missing node as not found and an unresolvable reference as invalid", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			_, err := f.graph().GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{Project: "platform", Node: "#9"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			_, err = f.graph().GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{Node: "#1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument), "a bare number needs a project")
		})
	})

	Describe("ListNodes", func() {
		It("filters by status, kind, labels, and parent, and pages by number", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			f.mustApply(ctx,
				createOpWith(&daggerv1.CreateNodeOp{Alias: "a", Title: "A", Kind: "bug", Labels: []string{"infra"}}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "b", Title: "B", Parent: "a"}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "c", Title: "C", Labels: []string{"infra", "urgent"}}),
			)
			f.mustApply(ctx, completeOp("#3", "done"))
			list := func(req *daggerv1.ListNodesRequest) *daggerv1.ListNodesResponse {
				GinkgoHelper()
				req.Project = "platform"
				res, err := f.graph().ListNodes(ctx, connect.NewRequest(req))
				Expect(err).NotTo(HaveOccurred())
				return res.Msg
			}

			Expect(numbers(list(&daggerv1.ListNodesRequest{}).GetNodes())).To(Equal([]int64{1, 2, 3}))
			Expect(numbers(list(&daggerv1.ListNodesRequest{Status: daggerv1.Status_STATUS_DONE}).GetNodes())).To(Equal([]int64{3}))
			Expect(numbers(list(&daggerv1.ListNodesRequest{Kind: "bug"}).GetNodes())).To(Equal([]int64{1}))
			Expect(numbers(list(&daggerv1.ListNodesRequest{Labels: []string{"infra"}}).GetNodes())).To(Equal([]int64{1, 3}))
			Expect(numbers(list(&daggerv1.ListNodesRequest{Parent: "#1"}).GetNodes())).To(Equal([]int64{2}))

			page := list(&daggerv1.ListNodesRequest{PageSize: 2})
			Expect(numbers(page.GetNodes())).To(Equal([]int64{1, 2}))
			Expect(page.GetNextPageToken()).NotTo(BeEmpty())
			last := list(&daggerv1.ListNodesRequest{PageSize: 2, PageToken: page.GetNextPageToken()})
			Expect(numbers(last.GetNodes())).To(Equal([]int64{3}))
			Expect(last.GetNextPageToken()).To(BeEmpty())

			_, err := f.graph().ListNodes(ctx, connect.NewRequest(&daggerv1.ListNodesRequest{Project: "platform", PageToken: "garbage"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			_, err = f.graph().ListNodes(ctx, connect.NewRequest(&daggerv1.ListNodesRequest{Project: "platform", PageSize: 5000}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeResourceExhausted))
		})
	})

	Describe("Explain", func() {
		It("returns structured reasons, and none once the node is ready", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			f.mustApply(ctx, createOp("a", "First"), createOp("b", "Second"), edgeOp("a", "b"))

			res, err := f.graph().Explain(ctx, connect.NewRequest(&daggerv1.ExplainRequest{Project: "platform", Node: "#2"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetNode().GetBlocked()).To(BeTrue())
			Expect(res.Msg.GetReasons()).To(HaveLen(1))
			blocked := res.Msg.GetReasons()[0].GetBlocked()
			Expect(blocked.GetBlocker().GetNumber()).To(BeEquivalentTo(1))
			Expect(blocked.GetBlocker().GetReady()).To(BeTrue(), "the blocker itself is ready")
			Expect(blocked.GetGate()).To(BeFalse())

			f.mustApply(ctx, completeOp("#1", "done"))
			res, err = f.graph().Explain(ctx, connect.NewRequest(&daggerv1.ExplainRequest{Project: "platform", Node: "#2"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetReasons()).To(BeEmpty())
			Expect(res.Msg.GetNode().GetReady()).To(BeTrue())
		})
	})

	Describe("Search", func() {
		It("finds nodes by their text with a marked snippet", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			f.mustApply(ctx, createOp("a", "Rotate the token store"), createOp("b", "Unrelated"))

			res, err := f.graph().Search(ctx, connect.NewRequest(&daggerv1.SearchRequest{Query: "token"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetHits()).To(HaveLen(1))
			Expect(res.Msg.GetHits()[0].GetNode().GetNumber()).To(BeEquivalentTo(1))
			Expect(res.Msg.GetHits()[0].GetSnippet()).To(ContainSubstring("<b>token</b>"))
			Expect(res.Msg.GetNextPageToken()).NotTo(BeEmpty(), "a caller pages until a page is empty")

			_, err = f.graph().Search(ctx, connect.NewRequest(&daggerv1.SearchRequest{Query: " "}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})

	Describe("CreateProject", func() {
		It("derives a lowercase slug from the title and disambiguates on collision", func(ctx SpecContext) {
			first := f.project(ctx, "OAuth Spike!")
			Expect(first.GetSlug()).To(Equal("oauth-spike"))
			Expect(first.GetId()).NotTo(BeEmpty())
			Expect(first.GetOrgId()).To(Equal(f.org))
			Expect(first.GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P2))
			Expect(first.GetNodeCounter()).To(BeEquivalentTo(1))

			second := f.project(ctx, "OAuth Spike!")
			Expect(second.GetSlug()).To(Equal("oauth-spike-2"))
			Expect(second.GetId()).NotTo(Equal(first.GetId()))

			res, err := f.graph().CreateProject(ctx, connect.NewRequest(&daggerv1.CreateProjectRequest{RequestId: newID(), Title: "Third", Slug: "My Slug/Here"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetProject().GetSlug()).To(Equal("my-slug-here"))
		})

		It("replays a repeated request id and lets an agent create a project", func(ctx SpecContext) {
			req := &daggerv1.CreateProjectRequest{RequestId: newID(), Title: "Once"}
			first, err := f.graph().CreateProject(ctx, connect.NewRequest(req))
			Expect(err).NotTo(HaveOccurred())
			again, err := f.graph().CreateProject(ctx, connect.NewRequest(req))
			Expect(err).NotTo(HaveOccurred())
			Expect(again.Msg.GetProject().GetId()).To(Equal(first.Msg.GetProject().GetId()))

			f.as(f.agent)
			res, err := f.graph().CreateProject(ctx, connect.NewRequest(&daggerv1.CreateProjectRequest{RequestId: newID(), Title: "Agent spike"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetProject().GetSlug()).To(Equal("agent-spike"))

			_, err = f.graph().CreateProject(ctx, connect.NewRequest(&daggerv1.CreateProjectRequest{RequestId: newID()}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument), "a title is required")
		})
	})

	Describe("GetProject", func() {
		It("resolves a slug or an id and reports a missing project as not found", func(ctx SpecContext) {
			created := f.project(ctx, "Platform")
			bySlug, err := f.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "platform"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(bySlug.Msg.GetProject().GetId()).To(Equal(created.GetId()))
			byID, err := f.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: created.GetId()}))
			Expect(err).NotTo(HaveOccurred())
			Expect(byID.Msg.GetProject().GetSlug()).To(Equal("platform"))

			_, err = f.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "nowhere"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			_, err = f.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})

	Describe("ListProjects", func() {
		It("hides archived projects unless asked, and pages", func(ctx SpecContext) {
			for _, title := range []string{"One", "Two", "Three"} {
				f.project(ctx, title)
			}
			_, err := f.graph().ArchiveProject(ctx, connect.NewRequest(&daggerv1.ArchiveProjectRequest{RequestId: newID(), Project: "two"}))
			Expect(err).NotTo(HaveOccurred())
			slugs := func(res *daggerv1.ListProjectsResponse) []string {
				out := []string{}
				for _, p := range res.GetProjects() {
					out = append(out, p.GetSlug())
				}
				return out
			}

			active, err := f.graph().ListProjects(ctx, connect.NewRequest(&daggerv1.ListProjectsRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(slugs(active.Msg)).To(Equal([]string{"three", "one"}))

			all, err := f.graph().ListProjects(ctx, connect.NewRequest(&daggerv1.ListProjectsRequest{IncludeArchived: true, PageSize: 2}))
			Expect(err).NotTo(HaveOccurred())
			Expect(slugs(all.Msg)).To(Equal([]string{"three", "two"}))
			rest, err := f.graph().ListProjects(ctx, connect.NewRequest(&daggerv1.ListProjectsRequest{IncludeArchived: true, PageSize: 2, PageToken: all.Msg.GetNextPageToken()}))
			Expect(err).NotTo(HaveOccurred())
			Expect(slugs(rest.Msg)).To(Equal([]string{"one"}))
			Expect(rest.Msg.GetNextPageToken()).To(BeEmpty())
		})
	})

	Describe("ArchiveProject", func() {
		It("makes the project read-only and is a user's to do", func(ctx SpecContext) {
			f.project(ctx, "Platform")
			f.mustApply(ctx, createOp("a", "Work"))

			f.as(f.agent)
			_, err := f.graph().ArchiveProject(ctx, connect.NewRequest(&daggerv1.ArchiveProjectRequest{RequestId: newID(), Project: "platform"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))

			f.as(f.user)
			res, err := f.graph().ArchiveProject(ctx, connect.NewRequest(&daggerv1.ArchiveProjectRequest{RequestId: newID(), Project: "platform"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetProject().GetArchivedAt()).NotTo(BeNil())

			_, err = f.apply(ctx, "platform", createOp("b", "Too late"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))

			explained, err := f.graph().Explain(ctx, connect.NewRequest(&daggerv1.ExplainRequest{Project: "platform", Node: "#1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(explained.Msg.GetReasons()).To(HaveLen(1))
			Expect(explained.Msg.GetReasons()[0].GetArchived().GetProject().GetSlug()).To(Equal("platform"))
		})
	})

	It("refuses a call the transport did not attribute", func(ctx SpecContext) {
		bare := start(server.Config{}, server.Services{Graph: server.NewGraphService(domain.NewService(f.pool, pol))}, f.pool)
		_, err := bare.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "platform"}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})
})

func ptr[T any](v T) *T { return &v }
