package domain

import (
	"context"
	"fmt"

	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/store"
)

// Every mutation is authorized by policy before its event is written, on
// the resolved operation and against the state it produced, inside the
// writing transaction. One denial rolls the whole patch back. The event
// then carries the version of the bundle that allowed it. See the
// Authorization section of docs/architecture.md.

// authorize asks the policy whether the actor may perform op on the node,
// given the change from before to after, and returns the version of the
// bundle that allowed it. Anything the input cannot be built from is a
// denial: an operation without a policy shape is not one that runs.
func (r *recorder) authorize(ctx context.Context, op, nodeID string, before, after any) (string, error) {
	in, err := r.policyInput(ctx, op, nodeID, before, after)
	if err != nil {
		return "", &policy.DeniedError{Action: op, Err: err}
	}
	if err := r.policy.Authorize(ctx, in); err != nil {
		return "", err
	}
	return r.policy.Version(), nil
}

// policyInput builds the policy's view of one operation from the typed
// before and after every event carries.
func (r *recorder) policyInput(ctx context.Context, op, nodeID string, before, after any) (policy.Input, error) {
	in := policy.Input{
		Principal: policy.Principal{ID: r.actor.PrincipalID, Kind: r.actor.PrincipalKind},
		Action:    op,
	}
	if r.lease != nil {
		in.Lease = leaseInfo(*r.lease)
	}
	switch op {
	case OpCreateNode:
		node, err := as[store.Node](after)
		if err != nil {
			return in, err
		}
		info, err := r.nodeInfo(ctx, node)
		if err != nil {
			return in, err
		}
		// The node's create event is the one about to be written.
		info.CreatedBy = r.actor.PrincipalID
		info.Lineage[0].CreatedBy = r.actor.PrincipalID
		in.After = &policy.Resource{Node: info}
	case OpUpdateNode, OpDropNode, OpReopenNode:
		prior, err := as[store.Node](before)
		if err != nil {
			return in, err
		}
		node, err := as[store.Node](after)
		if err != nil {
			return in, err
		}
		info, err := r.nodeInfo(ctx, node)
		if err != nil {
			return in, err
		}
		priorInfo, err := r.priorInfo(ctx, prior, info)
		if err != nil {
			return in, err
		}
		in.Resource.Node = priorInfo
		in.After = &policy.Resource{Node: info}
	case OpAddEdge:
		edge, err := as[store.Edge](after)
		if err != nil {
			return in, err
		}
		info, err := r.edgeInfo(ctx, edge)
		if err != nil {
			return in, err
		}
		in.Resource.Edge = info
		in.After = &policy.Resource{Edge: info}
	case OpRemoveEdge:
		edge, err := as[store.Edge](before)
		if err != nil {
			return in, err
		}
		info, err := r.edgeInfo(ctx, edge)
		if err != nil {
			return in, err
		}
		in.Resource.Edge = info
	case OpClaimNode, OpHeartbeatLease, OpReleaseLease, OpExpireLease:
		subject := before
		if op == OpClaimNode {
			subject = after
		}
		lease, err := as[store.Lease](subject)
		if err != nil {
			return in, err
		}
		node, err := r.tx.GetNode(ctx, store.GetNodeParams{OrgID: r.orgID, ID: nodeID})
		if err != nil {
			return in, fmt.Errorf("read node %s: %w", nodeID, err)
		}
		info, err := r.nodeInfo(ctx, node)
		if err != nil {
			return in, err
		}
		in.Resource.Node = info
		in.Resource.Lease = leaseInfo(lease)
		if op == OpHeartbeatLease || op == OpReleaseLease {
			in.Lease = leaseInfo(lease)
		}
	default:
		return in, fmt.Errorf("operation %q has no policy shape", op)
	}
	return in, nil
}

// as is the typed value an event carries, or an error naming what it
// carried instead.
func as[T any](v any) (T, error) {
	t, ok := v.(T)
	if !ok {
		return t, fmt.Errorf("event carries %T, not %T", v, t)
	}
	return t, nil
}

func leaseInfo(l store.Lease) *policy.Lease {
	return &policy.Lease{ID: l.ID, NodeID: l.NodeID, Holder: l.Holder}
}

// nodeInfo is the policy's view of a node as its row stands, with its
// lineage read from the parent relation.
func (r *recorder) nodeInfo(ctx context.Context, n store.Node) (*policy.Node, error) {
	lineage, err := r.lineage(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	if len(lineage) == 0 {
		return nil, fmt.Errorf("node %s has no row to authorize against", n.ID)
	}
	return &policy.Node{
		ID: n.ID, ProjectID: n.ProjectID, ParentID: n.ParentID, Kind: n.Kind, Status: n.Status,
		AssigneeKind: n.AssigneeKind, Assignee: n.Assignee, CreatedBy: lineage[0].CreatedBy, Lineage: lineage,
	}, nil
}

// priorInfo is the policy's view of a node as it stood before an update,
// given its view after. The row is already rewritten, so the lineage is
// recomputed from the earlier parent only when the parent changed.
func (r *recorder) priorInfo(ctx context.Context, prior store.Node, after *policy.Node) (*policy.Node, error) {
	info := *after
	info.ParentID, info.Kind, info.Status = prior.ParentID, prior.Kind, prior.Status
	info.AssigneeKind, info.Assignee = prior.AssigneeKind, prior.Assignee
	if !sameParent(prior.ParentID, after.ParentID) {
		info.Lineage = []policy.Ancestor{after.Lineage[0]}
		if prior.ParentID != nil {
			above, err := r.lineage(ctx, *prior.ParentID)
			if err != nil {
				return nil, err
			}
			info.Lineage = append(info.Lineage, above...)
		}
	}
	return &info, nil
}

func sameParent(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// lineage is the node and each ancestor up to the root, nearest first.
func (r *recorder) lineage(ctx context.Context, nodeID string) ([]policy.Ancestor, error) {
	rows, err := r.tx.NodeLineage(ctx, store.NodeLineageParams{OrgID: r.orgID, ID: nodeID})
	if err != nil {
		return nil, fmt.Errorf("read lineage of node %s: %w", nodeID, err)
	}
	lineage := make([]policy.Ancestor, 0, len(rows))
	for _, row := range rows {
		lineage = append(lineage, policy.Ancestor{ID: row.ID, CreatedBy: row.CreatedBy})
	}
	return lineage, nil
}

func (r *recorder) edgeInfo(ctx context.Context, e store.Edge) (*policy.Edge, error) {
	ends := [2]*policy.Node{}
	for i, id := range []string{e.FromNode, e.ToNode} {
		node, err := r.tx.GetNode(ctx, store.GetNodeParams{OrgID: r.orgID, ID: id})
		if err != nil {
			return nil, fmt.Errorf("read node %s: %w", id, err)
		}
		if ends[i], err = r.nodeInfo(ctx, node); err != nil {
			return nil, err
		}
	}
	return &policy.Edge{From: *ends[0], To: *ends[1], Kind: e.Kind}, nil
}
