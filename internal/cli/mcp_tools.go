package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/client"
	"github.com/ivy/dagger/internal/codec"
)

// The tools dg mcp offers. Each is one CLI command with its flags as
// arguments, described for the model that will call it: what the tool is
// for, when to reach for it, and the syntax its inputs take.

// refSyntax is the reference syntax, stated once and quoted by every tool
// that takes a reference.
const refSyntax = `A node reference is "#7" for node 7 of the current project, "slug#7" for
node 7 of project "slug" in this organization, or "org/slug#7" fully
qualified. A bare "#7" needs a current project; when none is configured,
qualify the reference.`

// filterSchema is the filter arguments ready and claim share.
func filterSchema() map[string]any {
	return map[string]any{
		"project":       stringProp("Project to look in, by slug or org/slug. Defaults to the current project."),
		"kind":          stringProp("Only nodes of this kind, such as task, bug, spike, or chore."),
		"assignee_kind": enumProp("Only nodes completable by this kind of principal.", enumChoices[daggerv1.AssigneeKind]()),
		"labels":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only nodes carrying every one of these labels."},
		"min_priority":  enumProp("Only nodes at this priority or more urgent.", enumChoices[daggerv1.Priority]()),
	}
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func enumProp(description string, values []string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": description}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// decodeArgs parses a tool's arguments strictly, so a misspelt argument is
// reported rather than silently ignored.
func decodeArgs(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return argsError(err, into)
	}
	return nil
}

// duration parses an optional duration argument such as "30s" or "5m".
func duration(name, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return 0, usage("%s: %q is not a duration such as 30s or 5m", name, value)
	}
	return d, nil
}

// filterArgs are the filters ready and claim share.
type filterArgs struct {
	Project      string   `json:"project"`
	Kind         string   `json:"kind"`
	AssigneeKind string   `json:"assignee_kind"`
	Labels       []string `json:"labels"`
	MinPriority  string   `json:"min_priority"`
}

func (f *filterArgs) resolve(a *app) (project string, kind daggerv1.AssigneeKind, prio daggerv1.Priority, err error) {
	project = f.Project
	if project == "" {
		if project, err = a.project(); err != nil {
			return "", 0, 0, err
		}
	}
	if kind, err = parseEnum[daggerv1.AssigneeKind]("assignee_kind", f.AssigneeKind); err != nil {
		return "", 0, 0, err
	}
	if prio, err = parseEnum[daggerv1.Priority]("min_priority", f.MinPriority); err != nil {
		return "", 0, 0, err
	}
	return project, kind, prio, nil
}

type pageArgs struct {
	PageSize  int32  `json:"page_size"`
	PageToken string `json:"page_token"`
}

func pageSchema(properties map[string]any) map[string]any {
	properties["page_size"] = map[string]any{"type": "integer", "description": "Maximum items to return. Defaults to the server's."}
	properties["page_token"] = stringProp("next_page_token of the previous page, to continue it.")
	return properties
}

// toolset builds the tools over this server's client.
func (s *mcpServer) toolset() []*mcpTool {
	a := s.app
	c := a.client
	return []*mcpTool{
		{
			name: "search",
			description: `Search node titles, bodies, results, and comments by text. Use it to find
work by topic, to check whether something already exists before creating
it, or to locate a node whose number you do not know. The query is
PostgreSQL websearch syntax: words, "quoted phrases", -excluded words, and
OR. Every project the caller may read is searched unless project narrows
it. Each hit carries a summary of the node and a snippet with the match
marked; show_node reads the body.`,
			schema: objectSchema(pageSchema(map[string]any{
				"query":            stringProp("Search text in websearch syntax, such as: parser \"error handling\" -legacy"),
				"project":          stringProp("Narrow to one project, by slug or org/slug. Defaults to the current project, or every project when none is configured."),
				"include_archived": map[string]any{"type": "boolean", "description": "Also search nodes of archived projects."},
				"view":             viewProp(daggerv1.View_VIEW_SUMMARY),
			}), "query"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Query           string `json:"query"`
					Project         string `json:"project"`
					IncludeArchived bool   `json:"include_archived"`
					View            string `json:"view"`
					pageArgs
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				if args.Query == "" {
					return nil, usage("query is required")
				}
				view, err := parseView("view", args.View, daggerv1.View_VIEW_SUMMARY)
				if err != nil {
					return nil, err
				}
				cfg, err := a.config()
				if err != nil {
					return nil, err
				}
				project := args.Project
				if project == "" {
					project = cfg.Project
				}
				return unwrap(c.Graph.Search(ctx, connect.NewRequest(&daggerv1.SearchRequest{
					Query: args.Query, Project: project, IncludeArchived: args.IncludeArchived,
					PageSize: args.PageSize, PageToken: args.PageToken, View: view,
				})))
			},
		},
		{
			name: "show_node",
			description: `Show one node in full: title, body (the prompt for whoever works it),
status, priority, labels, edges, parent, result if completed, current
lease if claimed, artifacts, and comments. Use it before starting work on
a node and whenever you need what a node says rather than a summary.

` + refSyntax,
			schema: objectSchema(map[string]any{
				"ref":  stringProp(`Node reference: "#7", "slug#7", or "org/slug#7".`),
				"view": viewProp(daggerv1.View_VIEW_FULL),
			}, "ref"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Ref  string `json:"ref"`
					View string `json:"view"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				project, ref, err := a.nodeArg(args.Ref)
				if err != nil {
					return nil, err
				}
				view, err := parseView("view", args.View, daggerv1.View_VIEW_FULL)
				if err != nil {
					return nil, err
				}
				return unwrap(c.Graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{Project: project, Node: ref, View: view})))
			},
		},
		{
			name: "explain",
			description: `Explain why a node is not ready. Each reason names what stands in the way:
an unfinished blocker (possibly a gate waiting on a person), an open
child, a live lease held by someone else, a status other than open, or an
archived project. No reasons means the node is ready. Use it when a node
you expected in the ready set is missing, or before waiting on one.

` + refSyntax,
			schema: objectSchema(map[string]any{
				"ref": stringProp(`Node reference: "#7", "slug#7", or "org/slug#7".`),
			}, "ref"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Ref string `json:"ref"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				project, ref, err := a.nodeArg(args.Ref)
				if err != nil {
					return nil, err
				}
				return unwrap(c.Graph.Explain(ctx, connect.NewRequest(&daggerv1.ExplainRequest{Project: project, Node: ref})))
			},
		},
		{
			name: "apply_patch",
			description: `Apply a patch: a list of operations applied in one transaction and
cycle-checked as a whole. This is the one way to change the graph, and the
tool to use when decomposing work into several nodes and linking them, or
for any change the single-purpose tools (complete, comment) do not cover.

Operations (the dagger.v1.PatchOp forms): create_node, update_node,
drop_node, complete_node, reopen_node, add_edge, remove_edge,
attach_artifact, add_comment, edit_comment, resolve_comment. Each names its
target in a field called "node", except add_edge and remove_edge, which use
"from" and "to". Reparenting and reprioritising are update_node fields
("parent", "priority"). A target is an existing node by reference
(` + "`#7`, `slug#7`, `org/slug#7`" + `), or a node created earlier in the
same patch by the alias its create_node declared. Aliases are what let one
patch build a whole subgraph atomically. Example, creating two nodes where
the second waits on the first and both hang under existing node #3:

  {"ops": [
    {"create_node": {"alias": "spike", "title": "Spike the parser", "parent": "#3"}},
    {"create_node": {"alias": "impl", "title": "Implement the parser", "parent": "#3", "blocked_by": ["spike"]}},
    {"add_edge": {"from": "spike", "to": "impl"}}
  ]}

An add_edge from A to B means A must finish before B is ready. The
response lists every node created or changed with its number, reference,
status, and readiness flags, maps each alias to the reference it became,
and counts the events written. It never echoes the bodies you sent; ask
for view "full" to get everything back, events included. A patch touching
a node you hold a lease on must carry that lease_id.`,
			schema: objectSchema(map[string]any{
				"ops": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "object"},
					"description": "The operations, in order, each a dagger.v1.PatchOp in JSON: an object with one key naming the operation, such as {\"create_node\": {...}} or {\"add_edge\": {\"from\": \"spike\", \"to\": \"#7\"}}.",
				},
				"project":    stringProp("Project the patch applies to and bare references resolve in. Defaults to the current project."),
				"lease_id":   stringProp("Fencing token of the lease you hold, required when the patch touches a leased node. Defaults to the one lease this session holds, if exactly one."),
				"request_id": stringProp("Idempotency key. Defaults to a fresh one; pass the same one to retry safely."),
				"view":       viewProp(daggerv1.View_VIEW_REFS),
			}, "ops"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				view, rest, err := splitView(raw, daggerv1.View_VIEW_REFS)
				if err != nil {
					return nil, err
				}
				req := &daggerv1.ApplyPatchRequest{}
				if err := codec.Unmarshal(rest, req); err != nil {
					return nil, decodeUsage(err)
				}
				req.View = view
				if len(req.GetOps()) == 0 {
					return nil, usage("ops is required and must hold at least one operation")
				}
				if req.GetProject() == "" {
					project, err := a.project()
					if err != nil {
						return nil, err
					}
					req.Project = project
				}
				req.RequestId = orMint(req.GetRequestId())
				req.LeaseId = s.leaseOr(req.GetLeaseId())
				return s.apply(ctx, req)
			},
		},
		{
			name: "ready",
			description: `List the ready set: nodes that are open, unblocked, unleased, and have no
open children, most urgent first and then oldest. Nothing is claimed. Use
it to see what could be worked on next; use claim to take one. Each node
is a summary (reference, title, kind, priority, labels); show_node reads
the body.`,
			schema: objectSchema(pageSchema(func() map[string]any {
				props := filterSchema()
				props["view"] = viewProp(daggerv1.View_VIEW_SUMMARY)
				return props
			}())),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					filterArgs
					pageArgs
					View string `json:"view"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				project, kind, prio, err := args.resolve(a)
				if err != nil {
					return nil, err
				}
				view, err := parseView("view", args.View, daggerv1.View_VIEW_SUMMARY)
				if err != nil {
					return nil, err
				}
				return unwrap(c.Work.Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{
					Project: project, Kind: args.Kind, AssigneeKind: kind, Labels: args.Labels, MinPriority: prio,
					PageSize: args.PageSize, PageToken: args.PageToken, View: view,
				})))
			},
		},
		{
			name: "claim",
			description: `Claim the most urgent ready node matching the filters, under a lease. Use
it when you are about to do the work: the lease makes the node yours and
keeps others off it. The response carries the node and its lease; the
lease id is the fencing token that complete, comment, and apply_patch must
carry for that node (it is used by default when it is the only one held).

The lease is renewed in the background for as long as this session runs,
so there is no need to call heartbeat yourself. If the session ends before
you complete the node, the lease is released with reason "crash" and the
node returns to the ready set. The response is empty when nothing matches;
wait blocks until something does, up to that long.`,
			schema: objectSchema(func() map[string]any {
				props := filterSchema()
				props["wait"] = stringProp(`How long to block for matching work, such as "30s" or "5m". Defaults to returning at once.`)
				props["ttl"] = stringProp(`Requested lease TTL, such as "15m". The server bounds it and applies its default when unset.`)
				props["agent_ref"] = stringProp("Where the attempt runs, recorded for placement affinity. Defaults to this host's name.")
				return props
			}()),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					filterArgs
					Wait     string `json:"wait"`
					TTL      string `json:"ttl"`
					AgentRef string `json:"agent_ref"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				project, kind, prio, err := args.resolve(a)
				if err != nil {
					return nil, err
				}
				wait, err := duration("wait", args.Wait)
				if err != nil {
					return nil, err
				}
				ttl, err := duration("ttl", args.TTL)
				if err != nil {
					return nil, err
				}
				agentRef := args.AgentRef
				if agentRef == "" {
					agentRef = s.agentRef
				}
				req := &daggerv1.ClaimNextRequest{
					RequestId: client.NewRequestID(), Project: project,
					Kind: args.Kind, AssigneeKind: kind, Labels: args.Labels, MinPriority: prio,
					Harness: s.harness, AgentRef: agentRef, SessionRef: s.sessionRef,
				}
				if ttl > 0 {
					req.Ttl = durationpb.New(ttl)
				}
				res, err := claimNext(ctx, c, req, wait)
				if err != nil {
					return nil, err
				}
				if res.GetLease() != nil {
					s.hold(res.GetLease(), res.GetNode())
				}
				return res, nil
			},
		},
		{
			name: "heartbeat",
			description: `Renew a lease explicitly. Leases claimed in this session are renewed
automatically, so this is only needed for a lease claimed elsewhere, or to
ask for a different TTL. A renewal that fails because the lease is no
longer live means the node has been reclaimed or expired: claim again
rather than continuing.`,
			schema: objectSchema(map[string]any{
				"lease_id": stringProp("The lease to renew. Defaults to the one lease this session holds, if exactly one."),
				"ttl":      stringProp(`Requested TTL from now, such as "15m". Defaults to the server's.`),
			}),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					LeaseID string `json:"lease_id"`
					TTL     string `json:"ttl"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				leaseID, err := s.requireLease(args.LeaseID)
				if err != nil {
					return nil, err
				}
				ttl, err := duration("ttl", args.TTL)
				if err != nil {
					return nil, err
				}
				req := &daggerv1.HeartbeatRequest{RequestId: client.NewRequestID(), LeaseId: leaseID}
				if ttl > 0 {
					req.Ttl = durationpb.New(ttl)
				}
				return unwrap(c.Work.Heartbeat(ctx, connect.NewRequest(req)))
			},
		},
		{
			name: "release",
			description: `End a lease without completing its node, returning the node to the ready
set for someone else. Give the reason an orchestrator should see: "gate"
when you filed a gate node and are stepping back to wait on a person,
"budget" when you stopped at a spending limit, "crash" when the attempt
failed, "preempted" when told to stop. Do not release a node you have
finished; complete it instead. Leases still held when this session ends
are released as "crash" automatically.`,
			schema: objectSchema(map[string]any{
				"lease_id": stringProp("The lease to release. Defaults to the one lease this session holds, if exactly one."),
				"reason": enumProp("Why the attempt ended.", shortNames(
					daggerv1.ReleaseReason_RELEASE_REASON_GATE,
					daggerv1.ReleaseReason_RELEASE_REASON_CRASH,
					daggerv1.ReleaseReason_RELEASE_REASON_BUDGET,
					daggerv1.ReleaseReason_RELEASE_REASON_PREEMPTED,
				)),
				"detail": stringProp("Free text expanding on the reason, recorded on the event: what was tried, what is left."),
			}, "reason"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					LeaseID string `json:"lease_id"`
					Reason  string `json:"reason"`
					Detail  string `json:"detail"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				reason, err := parseEnum[daggerv1.ReleaseReason]("reason", args.Reason)
				if err != nil {
					return nil, err
				}
				if reason == daggerv1.ReleaseReason_RELEASE_REASON_UNSPECIFIED {
					return nil, usage("reason is required: gate, crash, budget, or preempted")
				}
				leaseID, err := s.requireLease(args.LeaseID)
				if err != nil {
					return nil, err
				}
				res, err := unwrap(c.Work.Release(ctx, connect.NewRequest(&daggerv1.ReleaseRequest{
					RequestId: client.NewRequestID(), LeaseId: leaseID, Reason: reason, Detail: args.Detail,
				})))
				if err != nil {
					return nil, err
				}
				s.forget(leaseID)
				return res, nil
			},
		},
		{
			name: "complete",
			description: `Complete a node with a result, under the lease that holds it. The result
is what downstream agents and people read to build on the work, so say
what was produced and where: files, commits, decisions, anything left
undone. The lease is released as part of the same change. Use this when
the work is done; use release when it is not.

` + refSyntax,
			schema: objectSchema(map[string]any{
				"ref":      stringProp(`Node reference: "#7", "slug#7", or "org/slug#7".`),
				"result":   stringProp("Result body in markdown: what was produced and where."),
				"lease_id": stringProp("Fencing token of the lease held on the node. Defaults to the lease this session holds on it."),
				"view":     viewProp(daggerv1.View_VIEW_REFS),
			}, "ref", "result"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Ref     string `json:"ref"`
					Result  string `json:"result"`
					LeaseID string `json:"lease_id"`
					View    string `json:"view"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				ref, err := parseRef(args.Ref)
				if err != nil {
					return nil, err
				}
				project, err := a.projectFor(ref)
				if err != nil {
					return nil, err
				}
				view, err := parseView("view", args.View, daggerv1.View_VIEW_REFS)
				if err != nil {
					return nil, err
				}
				return s.apply(ctx, &daggerv1.ApplyPatchRequest{
					RequestId: client.NewRequestID(), Project: project, LeaseId: s.leaseOr(args.LeaseID),
					Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_CompleteNode{CompleteNode: &daggerv1.CompleteNodeOp{
						Node: ref.String(), Result: &daggerv1.Result{Body: args.Result},
					}}}},
					View: view,
				})
			},
		},
		{
			name: "comment",
			description: `Add a comment to a node: progress, a question for a person, a finding
worth keeping on the record. Comments live on the node so any later
attempt, by any agent or person, can read them. Use it for anything worth
saying that is not the final result.

` + refSyntax,
			schema: objectSchema(map[string]any{
				"ref":         stringProp(`Node reference: "#7", "slug#7", or "org/slug#7".`),
				"body":        stringProp("Comment body in markdown."),
				"in_reply_to": stringProp("Id of the comment this one replies to."),
				"lease_id":    stringProp("Fencing token of the lease held on the node, if any. Defaults to the lease this session holds on it."),
				"view":        viewProp(daggerv1.View_VIEW_REFS),
			}, "ref", "body"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Ref       string `json:"ref"`
					Body      string `json:"body"`
					InReplyTo string `json:"in_reply_to"`
					LeaseID   string `json:"lease_id"`
					View      string `json:"view"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				ref, err := parseRef(args.Ref)
				if err != nil {
					return nil, err
				}
				project, err := a.projectFor(ref)
				if err != nil {
					return nil, err
				}
				view, err := parseView("view", args.View, daggerv1.View_VIEW_REFS)
				if err != nil {
					return nil, err
				}
				return s.apply(ctx, &daggerv1.ApplyPatchRequest{
					RequestId: client.NewRequestID(), Project: project, LeaseId: s.leaseOr(args.LeaseID),
					Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_AddComment{AddComment: &daggerv1.AddCommentOp{
						Node: ref.String(), Body: args.Body, InReplyTo: args.InReplyTo,
					}}}},
					View: view,
				})
			},
		},
	}
}

// apply sends one patch. A held lease whose node the patch closed is
// forgotten: the server released it with the patch.
func (s *mcpServer) apply(ctx context.Context, req *daggerv1.ApplyPatchRequest) (proto.Message, error) {
	res, err := unwrap(s.app.client.Graph.ApplyPatch(ctx, connect.NewRequest(req)))
	if err != nil {
		return nil, err
	}
	if req.GetLeaseId() != "" {
		for _, h := range s.heldLeases() {
			if h.id != req.GetLeaseId() {
				continue
			}
			for _, n := range res.GetNodes() {
				if n.GetReference() == h.ref && n.GetStatus() != daggerv1.Status_STATUS_OPEN {
					s.forget(h.id)
				}
			}
		}
	}
	return res, nil
}

// leaseOr returns id, or the one lease this session holds when id is empty
// and exactly one is held. With none or several, the request goes without
// one and the server decides whether that matters.
func (s *mcpServer) leaseOr(id string) string {
	if id != "" {
		return id
	}
	if held := s.heldLeases(); len(held) == 1 {
		return held[0].id
	}
	return ""
}

// requireLease is leaseOr for the tools that cannot proceed without one.
func (s *mcpServer) requireLease(id string) (string, error) {
	if id = s.leaseOr(id); id != "" {
		return id, nil
	}
	held := s.heldLeases()
	if len(held) == 0 {
		return "", usage("lease_id is required: this session holds no lease to default to")
	}
	ids := make([]string, 0, len(held))
	for _, h := range held {
		ids = append(ids, h.id)
	}
	return "", usage("lease_id is required: this session holds %d leases (%s)", len(held), strings.Join(ids, ", "))
}
