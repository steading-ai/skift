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

// priorityIdiom is what the p0-p4 scale means, stated once. Dagger derives
// nothing from priority but the first sort key of the ready set; see
// docs/domain-model.md.
const priorityIdiom = `Priority is p0 to p4, p2 the default: advisory ordering within the ready
set, not a gate. A backlog where everything is p1 orders by age.`

// kindIdiom is what kind means, stated once.
const kindIdiom = `kind is a free string dagger never interprets, task the default, bug,
spike and chore the rest of a conventional, open vocabulary.`

// holdIdiom is what a hold means, stated once. Held work is absent from
// the ready set rather than finished, which is the conclusion an agent
// would otherwise draw from an empty answer.
const holdIdiom = `Held work is parked, not finished, and only a user may park or release
it. If you think work should be parked, say so in a comment.`

// filterSchema is the filter arguments ready and claim share.
func filterSchema() map[string]any {
	return map[string]any{
		"project":       stringProp("Project to look in, by slug or org/slug. Defaults to the current project."),
		"kind":          stringProp("Only nodes of this kind. Free-form, not an enum; conventionally task, bug, spike, or chore."),
		"assignee_kind": enumProp("Only nodes completable by this kind of principal. A node whose assignee kind is user is a gate, satisfied only by a user completing it; the dagger skill says what an agent may do with one.", enumChoices[daggerv1.AssigneeKind]()),
		"labels":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only nodes carrying every one of these labels."},
		"min_priority":  enumProp("Only nodes at this priority or more urgent. Advisory ordering, p2 the default: this filters the list, it does not decide what may be worked on.", enumChoices[daggerv1.Priority]()),
	}
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func enumProp(description string, values []string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": description}
}

func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// requestIDProp is the idempotency key every mutating tool takes, worded
// once.
func requestIDProp() map[string]any {
	return stringProp("Idempotency key. Defaults to a fresh one; pass the same one to retry safely.")
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
			description: `Search node titles, bodies, results and comments by text, across every
project you may read unless project narrows it. Hits rank by text match,
never by priority or readiness: a hit may be blocked, leased or closed,
and priority is only advisory ordering, p2 the default.`,
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
status, priority, labels, edges, parent, result, lease, artifacts and
comments. It carries the node's own result and never its predecessors':
it reports only how many inputs there are, and inputs returns them.`,
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
			name: "inputs",
			description: `Read the results of the nodes this one depends on: what a predecessor
produced, and where. This is the handoff, and show_node does not include it:
call it after claiming a node and before doing the work. An open
predecessor satisfies nothing and is not listed, and inputs are not
transitive.`,
			schema: objectSchema(map[string]any{
				"ref":  stringProp(`Node reference of the node whose inputs to read: "#7", "slug#7", or "org/slug#7".`),
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
				return unwrap(c.Graph.GetInputs(ctx, connect.NewRequest(&daggerv1.GetInputsRequest{Project: project, Node: ref, View: view})))
			},
		},
		{
			name: "node_list",
			description: `List a project's nodes by number, filtered by status, kind, label,
assignee kind and parent. It does not tell you what is ready: readiness
is derived from blockers, leases and open children, and ready answers it.
Every node is in scope whatever its status, so filter or this is the
whole backlog. Summaries, no bodies.`,
			schema: objectSchema(pageSchema(map[string]any{
				"project":       stringProp("Project to list, by slug or org/slug. Defaults to the current project."),
				"status":        enumProp("Only nodes in this status. Unset means every status, open and closed alike.", enumChoices[daggerv1.Status]()),
				"kind":          stringProp("Only nodes of this kind. Free-form, not an enum; conventionally task, bug, spike, or chore."),
				"assignee_kind": enumProp("Only nodes completable by this kind of principal. A node whose assignee kind is user is a gate, satisfied only by a user completing it; the dagger skill says what an agent may do with one.", enumChoices[daggerv1.AssigneeKind]()),
				"labels":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only nodes carrying every one of these labels."},
				"parent":        stringProp(`Only the children of this node, as a node reference such as "#7".`),
				"held":          boolProp("Only parked nodes: held themselves, contained in something held, or in a held project. This is how held work is found again, since it is absent from the ready set."),
				"view":          viewProp(daggerv1.View_VIEW_SUMMARY),
			})),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Project      string   `json:"project"`
					Status       string   `json:"status"`
					Kind         string   `json:"kind"`
					AssigneeKind string   `json:"assignee_kind"`
					Labels       []string `json:"labels"`
					Parent       string   `json:"parent"`
					Held         bool     `json:"held"`
					View         string   `json:"view"`
					pageArgs
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				project := args.Project
				if project == "" {
					var err error
					if project, err = a.project(); err != nil {
						return nil, err
					}
				}
				status, err := parseEnum[daggerv1.Status]("status", args.Status)
				if err != nil {
					return nil, err
				}
				assigneeKind, err := parseEnum[daggerv1.AssigneeKind]("assignee_kind", args.AssigneeKind)
				if err != nil {
					return nil, err
				}
				view, err := parseView("view", args.View, daggerv1.View_VIEW_SUMMARY)
				if err != nil {
					return nil, err
				}
				return unwrap(c.Graph.ListNodes(ctx, connect.NewRequest(&daggerv1.ListNodesRequest{
					Project: project, Status: status, Kind: args.Kind, AssigneeKind: assigneeKind,
					Labels: args.Labels, Parent: args.Parent, Held: args.Held,
					PageSize: args.PageSize, PageToken: args.PageToken, View: view,
				})))
			},
		},
		{
			name: "explain",
			description: `Explain why a node is not ready. Each reason names what stands in the
way: an unfinished blocker (possibly a gate waiting on a person), an open
child, a live lease held by someone else, a status other than open, or an
archived project. No reasons means it is ready.`,
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
			description: `Apply a patch: operations applied in one transaction, cycle-checked as a
whole, all or none. The one way to change the graph.

Operations, each shaped by the ops schema: create_node, update_node,
drop_node, complete_node, reopen_node, add_edge, remove_edge,
attach_artifact, add_comment, edit_comment, resolve_comment,
estimate_node, hold_node, unhold_node. Name a node
by reference (` + "`#7`, `slug#7`, `org/slug#7`" + `) or by an alias a create_node
declared earlier:

  {"ops": [
    {"create_node": {"alias": "spike", "title": "Spike it"}},
    {"create_node": {"alias": "impl", "title": "Do it", "blocked_by": ["spike"]}},
    {"add_edge": {"from": "spike", "to": "impl"}}
  ]}

add_edge from A to B means A must finish before B is ready; reverse it and
nothing errors, you just have the wrong graph. A parent is containment,
not dependency: a child takes its parent out of the ready set until every
child closes.

On create_node and update_node:
` + kindIdiom + `
` + priorityIdiom + `

drop_node takes a cascade, {"cascade": {"max_nodes": 5}}, refused naming
the real count when the subtree is larger. A patch touching a node you
hold a lease on must carry that lease_id.

estimate_node records an expected cost with a basis; the orchestrator
reports actuals, so no tool here does.

hold_node and unhold_node park work; the hold tool is the shorter way.`,
			schema: objectSchema(map[string]any{
				"ops":        opsProperty(),
				"project":    stringProp("Project the patch applies to and bare references resolve in. Defaults to the current project."),
				"lease_id":   stringProp("Fencing token of the lease you hold, required when the patch touches a leased node. Defaults to the one lease this session holds, if exactly one."),
				"request_id": requestIDProp(),
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
			description: `List the ready set: nodes open, unblocked, unheld, unleased and with no
open children, most urgent first and then oldest. Nothing is claimed. Use
it to see what could be worked on next; use claim to take one. Summaries,
no bodies.

An empty answer carries held_count: held work is parked, not done, and
explain says who parked it.

` + priorityIdiom + `
It is the first sort key here, and the only thing dagger does with it.`,
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
			description: `Claim the most urgent ready node matching the filters, under a lease
that makes it yours and keeps others off it. The lease id it
returns is the fencing token every later write to that node must carry —
complete, comment and apply_patch alike. Most urgent is priority order,
which is advisory: p2 is the default, and min_priority filters candidates
rather than gating them. A node whose assignee_kind is user is a gate,
satisfied only by a user completing it; the dagger skill says what an
agent may do with one.

The lease is renewed while this session runs, and released as "crash" if
it ends first.`,
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
automatically, so this is only for a lease claimed elsewhere, or a
different TTL. A renewal that fails means the lease is gone: claim again
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
set. Give the reason an orchestrator should see: "gate" when you filed a
gate node and are waiting on a person, "budget" at a limit, "crash" on
failure, "preempted" when told to stop. Complete a node you finished
instead.`,
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
is what downstream agents read to build on the work, so say what was
produced and where: files, commits, decisions, what is left undone. The
lease is released with it; use release when the work is not done.`,
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
worth keeping. Comments live on the node, so any later attempt by any
agent or person reads them. Use it for anything that is not the result.`,
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
		{
			name: "hold",
			description: `Park a node, or release it with release: true. It leaves the ready set
with its status untouched, everything it contains is parked with it, and
it still blocks what depends on it.

` + holdIdiom,
			schema: objectSchema(map[string]any{
				"node":       stringProp(`The node, as "#7", "slug#7", or "org/slug#7".`),
				"reason":     stringProp("Why the work is parked, or why it is coming back. Required: a hold nobody can explain is one nobody dares release."),
				"release":    boolProp("Release the node's hold instead of placing one."),
				"request_id": requestIDProp(),
			}, "node", "reason"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Node      string `json:"node"`
					Reason    string `json:"reason"`
					Release   bool   `json:"release"`
					RequestID string `json:"request_id"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				ref, err := parseRef(args.Node)
				if err != nil {
					return nil, err
				}
				project, err := a.projectFor(ref)
				if err != nil {
					return nil, err
				}
				return unwrap(c.Graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
					RequestId: orMint(args.RequestID), Project: project,
					Ops:  []*daggerv1.PatchOp{holdOp(ref.String(), args.Reason, args.Release)},
					View: daggerv1.View_VIEW_SUMMARY,
				})))
			},
		},
		{
			name: "focus",
			description: `Park everything outside the keep-set, or release that sweep with
release: true. Without project the keep-set names projects; with one it
names that project's nodes, and keeping a node keeps its whole lineage.

Focus replaces rather than stacks, so release needs no sweep id, and
touches only its own holds.`,
			schema: objectSchema(map[string]any{
				"keep":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "What stays live: project slugs when no project is given, otherwise node references within it. Everything else in scope is parked."},
				"project":    stringProp("Scope the sweep to this project, by slug or org/slug, so the keep-set names its nodes. Omit to sweep the organization's projects."),
				"reason":     stringProp("Why everything else is parked, or why it is coming back. Required."),
				"release":    boolProp("Release the scope's active sweep instead of placing one. Takes no keep-set."),
				"group":      stringProp("The sweep to release. Only needed to disambiguate; a scope has at most one active sweep."),
				"request_id": requestIDProp(),
			}, "reason"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Keep      []string `json:"keep"`
					Project   string   `json:"project"`
					Reason    string   `json:"reason"`
					Release   bool     `json:"release"`
					Group     string   `json:"group"`
					RequestID string   `json:"request_id"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				if args.Release {
					return unwrap(c.Graph.ClearFocus(ctx, connect.NewRequest(&daggerv1.ClearFocusRequest{
						RequestId: orMint(args.RequestID), Project: args.Project,
						Group: args.Group, Reason: args.Reason,
					})))
				}
				return unwrap(c.Graph.Focus(ctx, connect.NewRequest(&daggerv1.FocusRequest{
					RequestId: orMint(args.RequestID), Project: args.Project,
					Keep: args.Keep, Reason: args.Reason,
				})))
			},
		},
		{
			name: "project_list",
			description: `List the projects this caller may read, most recently active first, with
slug, title, advisory priority and node counter. Use it to find the slug
a reference needs, and whenever a bare "#7" cannot resolve for want of a
current project. It lists projects, not work; archived ones are read-only
and left out unless asked for.`,
			schema: objectSchema(pageSchema(map[string]any{
				"include_archived": boolProp("Also list archived projects, which are read-only."),
			})),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					IncludeArchived bool `json:"include_archived"`
					pageArgs
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				return unwrap(c.Graph.ListProjects(ctx, connect.NewRequest(&daggerv1.ListProjectsRequest{
					IncludeArchived: args.IncludeArchived, PageSize: args.PageSize, PageToken: args.PageToken,
				})))
			},
		},
		{
			name: "project_show",
			description: `Show one project: slug, title, advisory priority, node counter, the
repository it delivers to (vcs_url) if one is set, and when it was archived
if it was. Use it to confirm a slug before writing a
cross-project reference. It never returns the project's nodes. A project
is named by slug, "org/slug", or id, and a retired slug still resolves.`,
			schema: objectSchema(map[string]any{
				"project": stringProp(`The project, by slug, org/slug, or id. Defaults to the current project.`),
			}),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Project string `json:"project"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				project := args.Project
				if project == "" {
					var err error
					if project, err = a.project(); err != nil {
						return nil, err
					}
				}
				return unwrap(c.Graph.GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: project})))
			},
		},
		{
			name: "project_create",
			description: `Create a project: a namespace of nodes numbered from 1. Call
project_list first: a second project for work that belongs in an existing
one splits a graph that edges cannot rejoin.

The slug is derived from the title unless you give one, then lowercased
and disambiguated on collision, so the slug in the response is the one
references must use; assuming it from the title silently addresses the
wrong project.`,
			schema: objectSchema(map[string]any{
				"title":      stringProp("Human-readable name of the project. The slug is derived from it."),
				"slug":       stringProp("Requested slug. Defaults to one derived from the title; lowercased and disambiguated either way."),
				"priority":   enumProp("Advisory priority of the project as a whole. Dagger never sorts by it; p2 is the default.", enumChoices[daggerv1.Priority]()),
				"vcs_url":    stringProp("Clone URL of the repository the project delivers to, as an absolute URL. Dagger stores it for orchestrators and never reads it."),
				"request_id": requestIDProp(),
			}, "title"),
			call: func(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
				var args struct {
					Title     string `json:"title"`
					Slug      string `json:"slug"`
					Priority  string `json:"priority"`
					VCSURL    string `json:"vcs_url"`
					RequestID string `json:"request_id"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				if args.Title == "" {
					return nil, usage("title is required")
				}
				prio, err := parseEnum[daggerv1.Priority]("priority", args.Priority)
				if err != nil {
					return nil, err
				}
				return unwrap(c.Graph.CreateProject(ctx, connect.NewRequest(&daggerv1.CreateProjectRequest{
					RequestId: orMint(args.RequestID), Title: args.Title, Slug: args.Slug, Priority: prio, VcsUrl: args.VCSURL,
				})))
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
