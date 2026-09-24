package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
)

// Every mutation is authorized by policy before its event is written, on
// the resolved operation and against the state it produced, inside the
// writing transaction. One denial rolls the whole patch back, and is then
// recorded in policy_denials with the version of the bundle that refused
// it. The event an allowed mutation writes carries the version of the
// bundle that allowed it. See the Authorization section of
// docs/architecture.md.

// Denial is the error a refused operation ends its transaction with. It
// carries what the audit records once the transaction has rolled back,
// and unwraps to the [*policy.DeniedError] the policy gave.
type Denial struct {
	// Op is the operation refused, as events name operations. It may be
	// one the client never wrote, such as a drop a cascade produced.
	Op string
	// NodeID and ProjectID are the resolved target. The node may exist
	// only in the transaction that rolled back.
	NodeID    string
	ProjectID string
	// PolicyVersion is the bundle that refused it; empty when none was
	// loaded.
	PolicyVersion string
	Err           *policy.DeniedError
}

func (d *Denial) Error() string { return d.Err.Error() }

func (d *Denial) Unwrap() error { return d.Err }

// reasons is what the audit records as why: the policy's reasons, or the
// evaluation failure that stood in for a decision.
func (d *Denial) reasons() []string {
	if d.Err.Err != nil {
		return []string{d.Err.Err.Error()}
	}
	return d.Err.Reasons
}

// authorize asks the policy whether the actor may perform op on the node,
// given the change from before to after, and returns the version of the
// bundle that allowed it. Anything the input cannot be built from is a
// denial: an operation without a policy shape is not one that runs.
func (r *recorder) authorize(ctx context.Context, op, nodeID, projectID string, before, after any) (string, error) {
	denial := &Denial{Op: op, NodeID: nodeID, ProjectID: projectID, PolicyVersion: r.policy.Version()}
	in, err := r.policyInput(ctx, op, nodeID, before, after)
	if err != nil {
		denial.Err = &policy.DeniedError{Action: op, Err: err}
		return "", denial
	}
	if err := r.policy.Authorize(ctx, in); err != nil {
		if !errors.As(err, &denial.Err) {
			denial.Err = &policy.DeniedError{Action: op, Err: err}
		}
		return "", denial
	}
	return r.policy.Version(), nil
}

// authorizeRead asks the policy whether the actor may perform a read
// action on node, and returns a [*Denial] when it refuses. A read writes
// no event and rolls nothing back, so there is no transaction to audit the
// refusal in; the caller is told, and that is the whole of it.
func (r *recorder) authorizeRead(ctx context.Context, action string, node store.Node) error {
	denial := &Denial{Op: action, NodeID: node.ID, ProjectID: node.ProjectID, PolicyVersion: r.policy.Version()}
	info, err := r.nodeInfo(ctx, node)
	if err != nil {
		denial.Err = &policy.DeniedError{Action: action, Err: err}
		return denial
	}
	in := policy.Input{
		Principal: policy.Principal{ID: r.actor.PrincipalID, Kind: r.actor.PrincipalKind},
		Action:    action,
		Resource:  policy.Resource{Node: info},
	}
	if err := r.policy.Authorize(ctx, in); err != nil {
		if !errors.As(err, &denial.Err) {
			denial.Err = &policy.DeniedError{Action: action, Err: err}
		}
		return denial
	}
	return nil
}

// auditDenial records the denial err carries, if it carries one, in a
// transaction of its own: the one it ended has rolled back, and a record
// inside it would have gone with it. Any other error passes through. A
// denial whose record could not be written is still a denial, reported
// with the audit failure joined on.
func auditDenial(ctx context.Context, s *store.Store, orgID string, actor Actor, requestID *string, err error) error {
	var d *Denial
	if !errors.As(err, &d) {
		return err
	}
	audit := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.CreatePolicyDenial(ctx, store.CreatePolicyDenialParams{
			ID: newULID(), OrgID: orgID, ProjectID: optional(d.ProjectID), NodeID: optional(d.NodeID),
			PrincipalID: actor.PrincipalID, PrincipalKind: actor.PrincipalKind, IdentityScheme: actor.IdentityScheme,
			RequestID: requestID, Op: d.Op, Reasons: d.reasons(), PolicyVersion: d.PolicyVersion,
		})
		return err
	})
	if audit != nil {
		return errors.Join(err, fmt.Errorf("audit denial of %s: %w", d.Op, audit))
	}
	return err
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
	case OpUpdateNode, OpDropNode, OpReopenNode, OpCompleteNode, OpAttachArtifact, OpHoldNode, OpUnholdNode:
		prior, err := as[store.Node](before)
		if err != nil {
			return in, err
		}
		node, err := transitioned(op, after)
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
	case OpAddComment, OpEditComment, OpResolveComment:
		comment, err := as[store.Comment](after)
		if err != nil {
			return in, err
		}
		node, err := r.tx.GetNode(ctx, store.GetNodeParams{OrgID: r.orgID, ID: comment.NodeID})
		if err != nil {
			return in, fmt.Errorf("read node %s: %w", comment.NodeID, err)
		}
		info, err := r.nodeInfo(ctx, node)
		if err != nil {
			return in, err
		}
		// A comment changes nothing about its node, so the node stands
		// the same before and after; the comment is what changes.
		in.Resource.Node = info
		in.After = &policy.Resource{Node: info, Comment: commentInfo(comment)}
		if op != OpAddComment {
			prior, err := as[store.Comment](before)
			if err != nil {
				return in, err
			}
			in.Resource.Comment = commentInfo(prior)
		}
	case OpReportUsage, OpEstimateNode:
		usage, err := as[store.Usage](after)
		if err != nil {
			return in, err
		}
		node, err := r.tx.GetNode(ctx, store.GetNodeParams{OrgID: r.orgID, ID: usage.NodeID})
		if err != nil {
			return in, fmt.Errorf("read node %s: %w", usage.NodeID, err)
		}
		info, err := r.nodeInfo(ctx, node)
		if err != nil {
			return in, err
		}
		// A usage record changes nothing about its node; the record is
		// what is added.
		in.Resource.Node = info
		in.After = &policy.Resource{Node: info, Usage: usageInfo(usage)}
	default:
		return in, fmt.Errorf("operation %q has no policy shape", op)
	}
	return in, nil
}

func usageInfo(u store.Usage) *policy.Usage {
	return &policy.Usage{NodeID: u.NodeID, LeaseID: u.LeaseID, Principal: u.Principal, Kind: u.Kind}
}

func commentInfo(c store.Comment) *policy.Comment {
	return &policy.Comment{ID: c.ID, NodeID: c.NodeID, Author: c.Author, InReplyTo: c.InReplyTo, Resolved: c.ResolvedAt != nil}
}

// transitioned is the node a transition's after carries. A completion
// records the result beside the node and an attach records the artifact
// beside it; every other transition records the node alone.
func transitioned(op string, after any) (store.Node, error) {
	switch op {
	case OpCompleteNode:
		done, err := as[Completed](after)
		return done.Node, err
	case OpAttachArtifact:
		attached, err := as[Attached](after)
		return attached.Node, err
	}
	return as[store.Node](after)
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
