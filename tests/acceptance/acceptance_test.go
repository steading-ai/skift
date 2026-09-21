package acceptance_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/server/interceptor"
	"github.com/ivy/dagger/internal/testsupport"
)

// Every scenario starts from a clean database with dg serve freshly
// started over it, and speaks to the service only through the CLI and the
// MCP server. The person is on the laptop, over the tailnet; the agent is
// on the host, over the socket. See harness_test.go.

// blockedBy returns the numbers of the open blockers Explain names.
func blockedBy(res *daggerv1.ExplainResponse) []int64 {
	var out []int64
	for _, r := range res.GetReasons() {
		if b := r.GetBlocked(); b != nil {
			out = append(out, b.GetBlocker().GetNumber())
		}
	}
	return out
}

// gateFiled is the state after an agent filed a gate: the lease it holds
// on its node, and the gate.
type gateFiled struct {
	node  *daggerv1.Node
	lease *daggerv1.Lease
	gate  *daggerv1.Node
}

// fileGate has the agent claim the one node of platform and file a gate
// under it, blocking itself on the gate, without releasing the lease.
func fileGate(d *deployment) gateFiled {
	GinkgoHelper()
	claimed := d.host.claim()
	res := d.host.patch(request(platform,
		createOpWith(&daggerv1.CreateNodeOp{
			Alias: "gate", Parent: ref(claimed.GetNode()), Title: "Approve the production deploy",
			AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER,
		}),
		edgeOp("gate", ref(claimed.GetNode())),
	), "--lease", claimed.GetLease().GetId())
	var gate *daggerv1.Node
	for _, n := range res.GetNodes() {
		if n.GetAssigneeKind() == daggerv1.AssigneeKind_ASSIGNEE_KIND_USER {
			gate = n
		}
	}
	Expect(gate).NotTo(BeNil(), "the patch created no gate")
	return gateFiled{node: claimed.GetNode(), lease: claimed.GetLease(), gate: gate}
}

var _ = Describe("End to end", Label(testsupport.DBLabel), func() {
	Describe("Scenario: A project from nothing to a completed chain", func() {
		It("A project from nothing to a completed chain", func(ctx SpecContext) {
			d := deploy(ctx)

			By("given an empty database")
			Expect(d.health(ctx).Status).To(Equal("ok"))
			projects := &daggerv1.ListProjectsResponse{}
			d.laptop.call(projects, "project", "list", "--include-archived")
			Expect(projects.GetProjects()).To(BeEmpty())

			By("when I create a project and apply a patch creating a spike and two dependent tasks")
			project := d.laptop.createProject("Platform")
			Expect(project.GetSlug()).To(Equal(platform))
			res := d.laptop.patch(request(platform,
				createOpWith(&daggerv1.CreateNodeOp{Alias: "auth", Title: "Authentication", Body: "Synthesize the findings"}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "spike", Parent: "auth", Kind: "spike", Title: "Spike the OAuth flow"}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "server", Parent: "auth", Title: "Implement the server side", BlockedBy: []string{"spike"}}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "client", Parent: "auth", Title: "Implement the client side", BlockedBy: []string{"spike"}}),
			))
			Expect(res.GetAliases()).To(HaveLen(4))
			byAlias := map[string]*daggerv1.Node{}
			for _, n := range res.GetNodes() {
				for alias, r := range res.GetAliases() {
					if strings.HasSuffix(r, "#"+ref(n)[1:]) {
						byAlias[alias] = n
					}
				}
			}
			parent, spike, serverTask, clientTask := byAlias["auth"], byAlias["spike"], byAlias["server"], byAlias["client"]
			Expect(parent).NotTo(BeNil())
			Expect(spike).NotTo(BeNil())

			By("then only the spike is ready")
			Expect(numbers(d.laptop.ready())).To(Equal([]int64{spike.GetNumber()}))
			Expect(blockedBy(d.laptop.explain(ref(serverTask)))).To(Equal([]int64{spike.GetNumber()}))

			By("when an agent claims the spike, completes it with a result and an artifact")
			claimed := d.host.claim("--agent-ref", "grove-1")
			Expect(claimed.GetNode().GetNumber()).To(Equal(spike.GetNumber()))
			Expect(claimed.GetLease().GetHolder()).To(Equal(agentID))
			completed := d.host.patch(request(platform,
				completeOp(ref(spike), "Authorization code with PKCE; see the notes.",
					textArtifact("notes", "# OAuth spike\n\nUse the authorization code flow with PKCE."))),
				"--lease", claimed.GetLease().GetId())
			Expect(completed.GetNodes()[0].GetStatus()).To(Equal(daggerv1.Status_STATUS_DONE))
			shown := d.host.show(ref(spike))
			Expect(shown.GetResult().GetBody()).To(ContainSubstring("PKCE"))
			Expect(shown.GetResult().GetArtifacts()).To(HaveLen(1))
			Expect(shown.GetResult().GetArtifacts()[0].GetName()).To(Equal("notes"))

			By("then both tasks become ready")
			Expect(numbers(d.host.ready())).To(ConsistOf(serverTask.GetNumber(), clientTask.GetNumber()))

			By("when both are claimed and completed")
			for range 2 {
				c := d.host.claim("--agent-ref", "grove-2")
				d.host.call(&daggerv1.ApplyPatchResponse{}, "complete", "--project", platform, ref(c.GetNode()),
					"--lease", c.GetLease().GetId(), "--result", "Implemented and merged.")
			}

			By("then the parent node becomes ready for synthesis")
			Expect(numbers(d.laptop.ready())).To(Equal([]int64{parent.GetNumber()}))
			Expect(d.laptop.explain(ref(parent)).GetReasons()).To(BeEmpty())
		})
	})

	Describe("Scenario: An agent discovers a prerequisite mid-task", func() {
		It("An agent discovers a prerequisite mid-task", func(ctx SpecContext) {
			d := deploy(ctx)

			By("given an agent holding a lease on a node")
			d.laptop.createProject("Platform")
			d.laptop.patch(request(platform, createOp("parser", "Implement the parser")))
			claimed := d.host.claim()
			node := claimed.GetNode()
			Expect(node.GetClaimed()).To(BeTrue())

			By("when it applies a patch creating a prerequisite and blocking itself on it")
			res := d.host.patch(request(platform,
				createOpWith(&daggerv1.CreateNodeOp{Alias: "grammar", Parent: ref(node), Kind: "spike", Title: "Settle the grammar first"}),
				edgeOp("grammar", ref(node)),
			), "--lease", claimed.GetLease().GetId())
			var prerequisite *daggerv1.Node
			for _, n := range res.GetNodes() {
				if n.GetNumber() != node.GetNumber() {
					prerequisite = n
				}
			}
			Expect(prerequisite).NotTo(BeNil())

			By("then its node is no longer ready")
			Expect(numbers(d.host.ready())).NotTo(ContainElement(node.GetNumber()))
			explained := d.host.explain(ref(node))
			Expect(explained.GetNode().GetBlocked()).To(BeTrue())
			Expect(blockedBy(explained)).To(Equal([]int64{prerequisite.GetNumber()}))

			By("and the prerequisite is ready")
			Expect(numbers(d.host.ready())).To(Equal([]int64{prerequisite.GetNumber()}))
			Expect(d.host.explain(ref(prerequisite)).GetReasons()).To(BeEmpty())
		})
	})

	Describe("Scenario: An agent needs a human", func() {
		It("An agent needs a human", func(ctx SpecContext) {
			d := deploy(ctx)

			By("given an agent holding a lease")
			d.laptop.createProject("Platform")
			d.laptop.patch(request(platform, createOp("deploy", "Deploy the new release")))
			agent := d.host.mcp(platform)
			claimed := &daggerv1.ClaimNextResponse{}
			agent.call("claim", map[string]any{}).into(claimed)
			node := claimed.GetNode()
			Expect(node).NotTo(BeNil())

			By("when it files a user-assigned gate and releases in one operation")
			// The service has no single call that files and releases: the
			// gate is a patch under the lease, and the release follows it.
			// docs/domain-model.md asks for one atomic operation.
			filed := &daggerv1.ApplyPatchResponse{}
			agent.call("apply_patch", patchArgs(request(platform,
				createOpWith(&daggerv1.CreateNodeOp{
					Alias: "gate", Parent: ref(node), Title: "Approve the production deploy",
					AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER,
				}),
				edgeOp("gate", ref(node)),
			))).into(filed)
			var gate *daggerv1.Node
			for _, n := range filed.GetNodes() {
				if n.GetAssigneeKind() == daggerv1.AssigneeKind_ASSIGNEE_KIND_USER {
					gate = n
				}
			}
			Expect(gate).NotTo(BeNil())
			released := &daggerv1.ReleaseResponse{}
			agent.call("release", map[string]any{"reason": "gate", "detail": "waiting on approval of " + ref(gate)}).into(released)

			By(`then the lease is released with reason "gate"`)
			Expect(released.GetLease().GetId()).To(Equal(claimed.GetLease().GetId()))
			Expect(released.GetLease().GetReleaseReason()).To(Equal(daggerv1.ReleaseReason_RELEASE_REASON_GATE))
			Expect(d.host.show(ref(node)).GetLease()).To(BeNil())

			By("and the gate appears in the ready set filtered to user work")
			Expect(numbers(d.laptop.ready("--assignee-kind", "user"))).To(Equal([]int64{gate.GetNumber()}))

			By("and the agent's node is blocked")
			explained := d.host.explain(ref(node))
			Expect(explained.GetNode().GetBlocked()).To(BeTrue())
			Expect(blockedBy(explained)).To(Equal([]int64{gate.GetNumber()}))
			Expect(numbers(d.host.ready())).NotTo(ContainElement(node.GetNumber()))

			By("when the gate is completed by a user principal")
			completed := &daggerv1.ApplyPatchResponse{}
			d.laptop.call(completed, "complete", "--project", platform, ref(gate), "--result", "Approved.", "--view", "full")
			Expect(completed.GetEvents()).NotTo(BeEmpty())
			Expect(completed.GetEvents()[0].GetPrincipalKind()).To(Equal(daggerv1.PrincipalKind_PRINCIPAL_KIND_USER))

			By("then the agent's node is ready again")
			Expect(numbers(d.host.ready())).To(Equal([]int64{node.GetNumber()}))
			Expect(d.host.explain(ref(node)).GetReasons()).To(BeEmpty())
			agent.close()
		})
	})

	Describe("Scenario: A gate cannot be bypassed", func() {
		It("A gate cannot be bypassed", func(ctx SpecContext) {
			d := deploy(ctx)

			By("given an agent blocked by a gate it filed")
			d.laptop.createProject("Platform")
			d.laptop.patch(request(platform, createOp("deploy", "Deploy the new release")))
			filed := fileGate(d)
			Expect(blockedBy(d.host.explain(ref(filed.node)))).To(Equal([]int64{filed.gate.GetNumber()}))

			By("when it attempts to drop, complete, reassign or unlink that gate")
			attempts := map[string]*daggerv1.PatchOp{
				"drop":     dropOp(ref(filed.gate)),
				"complete": completeOp(ref(filed.gate), "Approving my own gate."),
				"reassign": updateOp(&daggerv1.UpdateNodeOp{Node: ref(filed.gate), AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_AGENT.Enum()}),
				"unlink":   removeEdgeOp(ref(filed.gate), ref(filed.node)),
			}
			for _, holding := range []bool{true, false} {
				for name, op := range attempts {
					var flags []string
					if holding {
						flags = []string{"--lease", filed.lease.GetId()}
					}

					By("then every attempt is denied: " + name)
					e := d.host.failPatch(request(platform, op), flags...)
					Expect(e.Code).To(Equal("permission_denied"), "%s (holding lease: %v): %s", name, holding, e.Message)
				}
				if holding {
					d.host.call(&daggerv1.ReleaseResponse{}, "release", "--project", platform, filed.lease.GetId(), "--reason", "gate")
				}
			}

			By("and its node stays blocked")
			explained := d.host.explain(ref(filed.node))
			Expect(explained.GetNode().GetBlocked()).To(BeTrue())
			Expect(blockedBy(explained)).To(Equal([]int64{filed.gate.GetNumber()}))
			gate := d.laptop.show(ref(filed.gate))
			Expect(gate.GetNode().GetStatus()).To(Equal(daggerv1.Status_STATUS_OPEN))
			Expect(gate.GetNode().GetAssigneeKind()).To(Equal(daggerv1.AssigneeKind_ASSIGNEE_KIND_USER))
			Expect(gate.GetEdges()).To(HaveLen(1))
			Expect(numbers(d.laptop.ready("--assignee-kind", "user"))).To(Equal([]int64{filed.gate.GetNumber()}))
		})
	})

	Describe("Scenario: Work resumes on the same agent when possible", func() {
		It("Work resumes on the same agent when possible", func(ctx SpecContext) {
			d := deploy(ctx)

			By("given a node whose previous lease recorded an agent_ref")
			d.laptop.createProject("Platform")
			d.laptop.patch(request(platform, createOp("migrate", "Migrate the schema")))
			cursor := d.host.cursorNow(platform)
			first := d.host.claim("--agent-ref", "grove-7")
			Expect(first.GetLease().GetAgentRef()).To(Equal("grove-7"))
			d.host.call(&daggerv1.ReleaseResponse{}, "release", "--project", platform, first.GetLease().GetId(), "--reason", "crash", "--detail", "sandbox died")

			By("when it is claimed again")
			second := d.host.claim("--agent-ref", "grove-9")
			Expect(second.GetNode().GetNumber()).To(Equal(first.GetNode().GetNumber()))
			Expect(second.GetLease().GetId()).NotTo(Equal(first.GetLease().GetId()))

			By("then the previous agent_ref is readable so an orchestrator can prefer it")
			stream := d.host.events(platform, cursor, "claim_node")
			var agentRefs []string
			for range 2 {
				ev := stream.nextEvent().GetEvent()
				Expect(ev.GetOp()).To(Equal("claim_node"))
				Expect(ev.GetNodeId()).To(Equal(first.GetNode().GetId()))
				agentRefs = append(agentRefs, ev.GetAfter().GetFields()["AgentRef"].GetStringValue())
			}
			Expect(agentRefs).To(Equal([]string{"grove-7", "grove-9"}))
			Expect(d.host.show(ref(first.GetNode())).GetLease().GetAgentRef()).To(Equal("grove-9"))
		})
	})

	Describe("Scenario: A retrieval consumer follows the graph", func() {
		It("A retrieval consumer follows the graph", func(ctx SpecContext) {
			d := deploy(ctx)
			d.laptop.createProject("Platform")

			By("given a consumer watching events from a cursor")
			cursor := d.laptop.cursorNow(platform)
			consumer := d.laptop.events(platform, cursor)

			By("when nodes, comments and results are written")
			var written []*daggerv1.Event
			created := d.laptop.patch(request(platform,
				createOpWith(&daggerv1.CreateNodeOp{Alias: "index", Title: "Index the corpus", Body: "Chunk and embed every document."}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "rank", Title: "Rank the results", Body: "Hybrid ranking over the index."}),
			))
			written = append(written, created.GetEvents()...)
			index, rank := created.GetNodes()[0], created.GetNodes()[1]
			commented := d.laptop.patch(request(platform, commentOp(ref(index), "Start with the design notes.")))
			written = append(written, commented.GetEvents()...)

			By("then it receives every event once")
			// received is every event in delivery order; seen indexes it by id.
			var received []*daggerv1.Event
			seen := map[string]*daggerv1.Event{}
			receive := func(s *eventStream, n int) (last string) {
				GinkgoHelper()
				for range n {
					res := s.nextEvent()
					ev := res.GetEvent()
					Expect(seen).NotTo(HaveKey(ev.GetId()), "event %s (%s) was delivered twice", ev.GetId(), ev.GetOp())
					seen[ev.GetId()] = ev
					received = append(received, ev)
					last = res.GetCursor()
				}
				return last
			}
			resumeFrom := receive(consumer, len(written))
			consumer.quiet()

			By("and resumes after a restart")
			consumer.stop()
			claimed := d.host.claim()
			Expect(claimed.GetNode().GetNumber()).To(Equal(index.GetNumber()))
			completed := d.host.patch(request(platform, completeOp(ref(index), "Indexed 1,204 documents.")), "--lease", claimed.GetLease().GetId())
			written = append(written, completed.GetEvents()...)
			resumed := d.laptop.events(platform, resumeFrom)
			receive(resumed, len(completed.GetEvents())+1)
			resumed.quiet()
			for _, ev := range written {
				Expect(seen).To(HaveKey(ev.GetId()), "event %s (%s) never arrived", ev.GetId(), ev.GetOp())
			}
			var ops []string
			for _, ev := range received {
				ops = append(ops, ev.GetOp())
			}
			Expect(ops).To(Equal([]string{"create_node", "create_node", "add_comment", "claim_node", "complete_node", "release_lease"}))

			By("and can rebuild the current state from what it received")
			type rebuilt struct {
				title    string
				status   string
				comments []string
			}
			state := map[string]*rebuilt{}
			nodeOf := func(id string) *rebuilt {
				if state[id] == nil {
					state[id] = &rebuilt{}
				}
				return state[id]
			}
			for _, ev := range received {
				after := ev.GetAfter().GetFields()
				switch ev.GetOp() {
				case "create_node":
					n := nodeOf(ev.GetNodeId())
					n.title = after["Title"].GetStringValue()
					n.status = after["Status"].GetStringValue()
				case "complete_node":
					// A completion's after is the node with its result.
					nodeOf(ev.GetNodeId()).status = after["node"].GetStructValue().GetFields()["Status"].GetStringValue()
				case "add_comment":
					n := nodeOf(ev.GetNodeId())
					n.comments = append(n.comments, after["Body"].GetStringValue())
				}
			}
			for _, n := range []*daggerv1.Node{index, rank} {
				shown := d.laptop.show(ref(n))
				Expect(state).To(HaveKey(n.GetId()))
				Expect(state[n.GetId()].title).To(Equal(shown.GetNode().GetTitle()))
				Expect("STATUS_" + strings.ToUpper(state[n.GetId()].status)).To(Equal(shown.GetNode().GetStatus().String()))
				var bodies []string
				for _, c := range shown.GetComments() {
					bodies = append(bodies, c.GetBody())
				}
				Expect(state[n.GetId()].comments).To(Equal(bodies))
			}
		})
	})

	Describe("Scenario: A laptop session works over the tailnet", func() {
		It("A laptop session works over the tailnet", func(ctx SpecContext) {
			d := deploy(ctx)

			By("given the MCP server configured against the tailnet address")
			d.laptop.createProject("Platform")
			d.laptop.patch(request(platform, createOp("review", "Review the launch checklist")))
			cursor := d.laptop.cursorNow(platform)
			session := d.laptop.mcp(platform)

			By("when a session claims, comments, and completes a node")
			claimed := &daggerv1.ClaimNextResponse{}
			session.call("claim", map[string]any{}).into(claimed)
			Expect(claimed.GetNode()).NotTo(BeNil())
			Expect(claimed.GetLease().GetHolder()).To(Equal(userID))
			Expect(claimed.GetLease().GetHarness()).To(Equal("claude-code"))
			node := ref(claimed.GetNode())
			commented := &daggerv1.ApplyPatchResponse{}
			session.call("comment", map[string]any{"ref": node, "body": "Checked every item."}).into(commented)
			completed := &daggerv1.ApplyPatchResponse{}
			session.call("complete", map[string]any{"ref": node, "result": "All clear for launch."}).into(completed)
			Expect(completed.GetNodes()[0].GetStatus()).To(Equal(daggerv1.Status_STATUS_DONE))
			session.close()

			By("then the events record the tailscale identity scheme")
			stream := d.laptop.events(platform, cursor)
			var schemes, ops []string
			for len(ops) < 3 {
				ev := stream.nextEvent().GetEvent()
				ops = append(ops, ev.GetOp())
				schemes = append(schemes, ev.GetIdentityScheme())
				Expect(ev.GetPrincipal()).To(Equal(userID))
				Expect(ev.GetPrincipalKind()).To(Equal(daggerv1.PrincipalKind_PRINCIPAL_KIND_USER))
			}
			Expect(ops).To(Equal([]string{"claim_node", "add_comment", "complete_node"}))
			Expect(schemes).To(HaveEach(interceptor.SchemeTailscale))
			for _, ev := range append(commented.GetEvents(), completed.GetEvents()...) {
				Expect(ev.GetIdentityScheme()).To(Equal(interceptor.SchemeTailscale))
			}
		})
	})

	Describe("Scenario: A person parks work the orchestrator would otherwise take", func() {
		It("A person parks work the orchestrator would otherwise take", func(ctx SpecContext) {
			d := deploy(ctx)

			By("given two independent nodes in a project")
			d.laptop.createProject("Platform")
			res := d.laptop.patch(request(platform,
				createOp("keep", "The work that matters this week"),
				createOp("park", "The work that does not"),
			))
			keep, park := res.GetNodes()[0], res.GetNodes()[1]
			Expect(numbers(d.host.ready())).To(ConsistOf(keep.GetNumber(), park.GetNumber()))

			By("when a person focuses the project on one of them")
			focus := &daggerv1.FocusResponse{}
			d.laptop.call(focus, "focus", "--project", platform, "--reason", "this week", ref(keep))
			Expect(focus.GetHeldCount()).To(Equal(int32(1)))

			By("then the orchestrator is offered only the kept work")
			Expect(numbers(d.host.ready())).To(Equal([]int64{keep.GetNumber()}))

			By("and the parked node says who parked it and why, while staying open")
			explained := d.host.explain(ref(park))
			Expect(explained.GetNode().GetStatus()).To(Equal(daggerv1.Status_STATUS_OPEN))
			Expect(explained.GetNode().GetHeld()).To(BeTrue())
			Expect(explained.GetReasons()).To(HaveLen(1))
			held := explained.GetReasons()[0].GetHeld()
			Expect(held.GetSelf()).To(BeTrue())
			Expect(held.GetHold().GetReason()).To(Equal("this week"))
			Expect(held.GetHold().GetBy()).NotTo(BeEmpty())

			By("and an agent may not park work of its own")
			_, stderr, code := d.host.run("", "hold", "--project", platform, ref(keep), "--reason", "not now")
			Expect(code).NotTo(Equal(0))
			Expect(stderr).To(ContainSubstring("no rule permits agent hold_node"))

			By("when the sweep is cleared without naming it")
			cleared := &daggerv1.ClearFocusResponse{}
			d.laptop.call(cleared, "focus", "--project", platform, "--clear", "--reason", "week over")
			Expect(cleared.GetReleasedCount()).To(Equal(int32(1)))

			By("then the parked work is offered again")
			Expect(numbers(d.host.ready())).To(ConsistOf(keep.GetNumber(), park.GetNumber()))
		})
	})

	Describe("Scenario: Search finds work across projects", func() {
		It("Search finds work across projects", func(ctx SpecContext) {
			d := deploy(ctx)

			By("given nodes in three projects including one archived")
			ids := map[string]string{}
			for _, title := range []string{"Alpha", "Beta", "Gamma"} {
				p := d.laptop.createProject(title)
				ids[p.GetSlug()] = p.GetId()
				d.laptop.patch(request(p.GetSlug(), createOpWith(&daggerv1.CreateNodeOp{
					Alias: "n", Title: "Rotate the signing key", Body: "The signing key for " + title + " expires soon.",
				})))
			}
			archived := &daggerv1.ArchiveProjectResponse{}
			d.laptop.call(archived, "project", "archive", "gamma")
			Expect(archived.GetProject().GetArchivedAt()).NotTo(BeNil())

			By("when I search a term appearing in all three")
			hits := &daggerv1.SearchResponse{}
			d.laptop.call(hits, "search", "signing key")

			By("then results from the two active projects are returned by default")
			var projects []string
			for _, h := range hits.GetHits() {
				projects = append(projects, h.GetNode().GetProjectId())
			}
			Expect(projects).To(ConsistOf(ids["alpha"], ids["beta"]))

			all := &daggerv1.SearchResponse{}
			d.laptop.call(all, "search", "signing key", "--include-archived")
			Expect(all.GetHits()).To(HaveLen(3), "and the archived one only on request")
		})
	})

	Describe("Scenario: Claiming with no project turns across every project", func() {
		It("Claiming with no project turns across every project", func(ctx SpecContext) {
			d := deploy(ctx)

			By("given one ready node in each of two projects")
			alpha := d.laptop.createProject("Alpha")
			beta := d.laptop.createProject("Beta")
			d.laptop.patch(request(alpha.GetSlug(), createOp("a", "Alpha's work")))
			d.laptop.patch(request(beta.GetSlug(), createOp("b", "Beta's work")))

			By("when no project is named, ready draws from just one project's turn")
			ready := &daggerv1.ReadyResponse{}
			d.host.call(ready, "ready")
			Expect(ready.GetNodes()).To(HaveLen(1))
			first := ready.GetNodes()[0].GetProjectId()
			Expect(first).To(BeElementOf(alpha.GetId(), beta.GetId()))

			By("and claiming takes that same project's node")
			claimed := &daggerv1.ClaimNextResponse{}
			d.host.call(claimed, "claim", "--harness", "acceptance")
			Expect(claimed.GetNode().GetProjectId()).To(Equal(first))

			By("then the turn moves to the other project")
			ready = &daggerv1.ReadyResponse{}
			d.host.call(ready, "ready")
			Expect(ready.GetNodes()).To(HaveLen(1))
			second := ready.GetNodes()[0].GetProjectId()
			Expect(second).NotTo(Equal(first))
			Expect(second).To(BeElementOf(alpha.GetId(), beta.GetId()))

			claimed = &daggerv1.ClaimNextResponse{}
			d.host.call(claimed, "claim", "--harness", "acceptance")
			Expect(claimed.GetNode().GetProjectId()).To(Equal(second))

			By("and naming one project claims only from it")
			d.laptop.patch(request(alpha.GetSlug(), createOp("a2", "More of alpha's work")))
			fromBeta := &daggerv1.ClaimNextResponse{}
			d.host.call(fromBeta, "claim", "--project", beta.GetSlug(), "--harness", "acceptance")
			Expect(fromBeta.GetNode()).To(BeNil(), "beta has nothing ready")

			fromAlpha := &daggerv1.ClaimNextResponse{}
			d.host.call(fromAlpha, "claim", "--project", alpha.GetSlug(), "--harness", "acceptance")
			Expect(fromAlpha.GetNode().GetProjectId()).To(Equal(alpha.GetId()))
		})
	})
})
