package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/steading-ai/skift/internal/store"
)

// Reading nodes for the wire: one node with everything attached to it, a
// filtered page of a project's nodes, and the derived flags every node
// carries when it leaves the service. Nothing here is stored; blocked,
// ready, claimed, and human_touched are computed from the same conditions
// Ready checks, so the flags and the ready set cannot disagree.

// Derived is the state a node carries that is computed rather than
// stored. See the Node section of docs/domain-model.md.
type Derived struct {
	// Blocked: an open node blocks this one through a `blocks` edge, or a
	// gate blocking it has not been completed.
	Blocked bool
	// Ready: open, not blocked, not held, no live lease, no open
	// children, and its project is neither archived nor held.
	Ready bool
	// Claimed: a live lease holds the node.
	Claimed bool
	// Held: the node, a node containing it, or its project carries a
	// hold. See the Hold section of docs/domain-model.md.
	Held bool
	// HumanTouched: a user principal has written to the node at least
	// once, according to the event log.
	HumanTouched bool
	// Ref: the node's reference as a request names it, `slug#7`, from the
	// project's current slug and the node's number.
	Ref string
}

// NodeState is a node with its derived flags.
type NodeState struct {
	Node
	Derived
}

// derive computes the derived flags of nodes, keyed by node id.
func derive(ctx context.Context, tx store.Tx, orgID string, nodes []store.Node) (map[string]Derived, error) {
	out := make(map[string]Derived, len(nodes))
	if len(nodes) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	derived, err := tx.DeriveNodes(ctx, orgID, ids)
	if err != nil {
		return nil, fmt.Errorf("derive node state: %w", err)
	}
	for _, d := range derived {
		out[d.ID] = Derived{
			Blocked:      d.Blocked,
			Ready:        d.Ready,
			Claimed:      d.Claimed,
			Held:         d.Held || d.ProjectHeld,
			HumanTouched: d.HumanTouched,
			Ref:          NodeRef{Project: d.Slug, Number: d.Number}.String(),
		}
	}
	return out, nil
}

// annotate pairs each node with its derived flags, in the order given.
func annotate(ctx context.Context, tx store.Tx, orgID string, nodes []store.Node) ([]NodeState, error) {
	derived, err := derive(ctx, tx, orgID, nodes)
	if err != nil {
		return nil, err
	}
	out := make([]NodeState, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, NodeState{Node: n, Derived: derived[n.ID]})
	}
	return out, nil
}

// NodeView is one node with everything attached to it, as GetNode returns
// it.
type NodeView struct {
	NodeState
	// Edges are every edge into or out of the node.
	Edges []Edge
	// Result is the node's current result while it is done, else nil.
	Result *StoredResult
	// Lease is the live lease on the node, else nil.
	Lease *Lease
	// Artifacts are attached to the node, in attachment order.
	Artifacts []StoredArtifact
	// Comments are the node's comments, oldest first, with revisions.
	Comments []CommentHistory
	// Inputs is how much the node's inputs come to. The results
	// themselves are [ReadInputs]'s to return.
	Inputs InputsHint
	// CommentsHint is what the node's unresolved comments come to,
	// whatever the view the caller will project this through.
	CommentsHint CommentsHint
	// Estimate is the node's current estimate, else nil.
	Estimate *Usage
	// Usage is what the node and its descendants cost and are expected
	// to cost, computed on this read.
	Usage *Rollup
}

// GetNode returns the node ref names with its relations and content.
// project is what a bare `#7` resolves in, and may be empty when ref
// carries its own.
func (g *Service) GetNode(ctx context.Context, orgID, project, ref string) (*NodeView, error) {
	var out *NodeView
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		resolved, err := resolveNode(ctx, tx, orgID, project, ref)
		if err != nil {
			return err
		}
		out, err = readNode(ctx, tx, orgID, resolved.ID)
		return err
	})
	return out, err
}

func readNode(ctx context.Context, tx store.Tx, orgID, nodeID string) (*NodeView, error) {
	node, err := tx.GetNode(ctx, store.GetNodeParams{OrgID: orgID, ID: nodeID})
	if errors.Is(err, store.ErrNoRows) {
		return nil, ErrNodeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read node %s: %w", nodeID, err)
	}
	states, err := annotate(ctx, tx, orgID, []store.Node{node})
	if err != nil {
		return nil, err
	}
	v := &NodeView{NodeState: states[0], Edges: []store.Edge{}}

	into, err := tx.ListEdgesInto(ctx, store.ListEdgesIntoParams{OrgID: orgID, ToNode: nodeID})
	if err != nil {
		return nil, fmt.Errorf("read edges into node %s: %w", nodeID, err)
	}
	outOf, err := tx.ListEdgesOutOf(ctx, store.ListEdgesOutOfParams{OrgID: orgID, FromNode: nodeID})
	if err != nil {
		return nil, fmt.Errorf("read edges out of node %s: %w", nodeID, err)
	}
	v.Edges = append(append(v.Edges, into...), outOf...)

	results, err := tx.ListResultsForNode(ctx, store.ListResultsForNodeParams{OrgID: orgID, NodeID: nodeID})
	if err != nil {
		return nil, fmt.Errorf("read results of node %s: %w", nodeID, err)
	}
	if node.Status == "done" && len(results) > 0 {
		v.Result = &results[0]
	}

	leases, err := tx.LiveLeaseOf(ctx, orgID, nodeID)
	if err != nil {
		return nil, err
	}
	if len(leases) > 0 {
		v.Lease = &leases[0]
	}

	if v.Artifacts, err = readArtifacts(ctx, tx, orgID, nodeID); err != nil {
		return nil, err
	}
	if v.Comments, err = Comments(ctx, tx, orgID, nodeID, 0, 0); err != nil {
		return nil, err
	}
	if v.Inputs, err = InputsSize(ctx, tx, orgID, nodeID); err != nil {
		return nil, err
	}
	if v.CommentsHint, err = CommentsSummary(ctx, tx, orgID, nodeID); err != nil {
		return nil, err
	}
	if v.Estimate, err = CurrentEstimate(ctx, tx, orgID, nodeID); err != nil {
		return nil, err
	}
	if v.Usage, err = SubtreeRollup(ctx, tx, orgID, nodeID); err != nil {
		return nil, err
	}
	return v, nil
}

// NodeListQuery is one page of nodes, filtered. Every field is optional
// and narrows the page; Projects empty means every project of the
// organization.
type NodeListQuery struct {
	// Projects keeps nodes of any of these projects, each named by slug,
	// `org/slug`, or id. Empty means every project of the organization.
	Projects []string
	// Status keeps nodes in one stored status: open, done, or dropped.
	Status string
	// Kinds keeps nodes of any of these kinds.
	Kinds []string
	// AssigneeKind keeps nodes completable by that kind of principal, as
	// [ReadyQuery.AssigneeKind] does.
	AssigneeKind string
	// Labels keeps nodes carrying every one of them.
	Labels []string
	// AnyLabels keeps nodes carrying at least one of them.
	AnyLabels []string
	// MinPriority keeps nodes at this priority or more urgent, compared
	// within each project as [ReadyQuery.MinPriority] does.
	MinPriority string
	// Parent keeps the children of one node, as a node reference. A bare
	// `#7` resolves within Projects when it names exactly one, and
	// otherwise must qualify its own.
	Parent string
	// Held keeps only the nodes a hold parks, whether the hold is on the
	// node, on something containing it, or on its project. It is how
	// parked work is found again.
	Held bool
	// Limit caps the page, at most [Limits.PageSize]. Zero takes that
	// ceiling.
	Limit int
	// Offset skips that many nodes, for the next page.
	Offset int
}

// filter is q's filters as the store applies them, over the resolved
// projects and parent, or why one of them is not valid.
func (q NodeListQuery) filter(projects []string, parentID string) (store.NodeFilter, error) {
	switch q.Status {
	case "", "open", "done", "dropped":
	default:
		return store.NodeFilter{}, fmt.Errorf("%w: status %q is not open, done, or dropped", ErrInvalidFilter, q.Status)
	}
	switch q.AssigneeKind {
	case "", AssigneeAgent, AssigneeUser:
	default:
		return store.NodeFilter{}, fmt.Errorf("%w: assignee kind %q is not agent or user", ErrInvalidFilter, q.AssigneeKind)
	}
	if q.MinPriority != "" && !validPriority(q.MinPriority) {
		return store.NodeFilter{}, fmt.Errorf("%w: priority %q is not p0 through p4", ErrInvalidFilter, q.MinPriority)
	}
	return store.NodeFilter{
		Projects: projects, Status: q.Status, Kinds: q.Kinds, AssigneeKind: q.AssigneeKind,
		Labels: q.Labels, AnyLabels: q.AnyLabels, MinPriority: q.MinPriority, Held: q.Held, ParentID: parentID,
	}, nil
}

// ListNodes returns one page of the nodes matching q, ordered by project
// then by number so a page is stable across any number of projects.
func (g *Service) ListNodes(ctx context.Context, orgID string, q NodeListQuery) ([]NodeState, error) {
	if q.Offset < 0 {
		return nil, errors.New("offset is negative")
	}
	var out []NodeState
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		scope, err := resolveScope(ctx, tx, orgID, q.Projects)
		if err != nil {
			return err
		}
		var parentID string
		if q.Parent != "" {
			var project *store.Project
			if len(scope) == 1 {
				p, err := tx.GetProject(ctx, store.GetProjectParams{OrgID: orgID, ID: scope[0]})
				if err != nil {
					return fmt.Errorf("read project %s: %w", scope[0], err)
				}
				project = &p
			}
			parent, err := NewResolver(tx, orgID, project).Resolve(ctx, q.Parent)
			if err != nil {
				return err
			}
			parentID = parent.ID
		}
		f, err := q.filter(scope, parentID)
		if err != nil {
			return err
		}
		limit, err := boundPageSize(q.Limit, ActiveLimits())
		if err != nil {
			return err
		}
		nodes, err := tx.FilterNodes(ctx, orgID, f, limit, q.Offset)
		if err != nil {
			return fmt.Errorf("list nodes: %w", err)
		}
		out, err = annotate(ctx, tx, orgID, nodes)
		return err
	})
	return out, err
}
