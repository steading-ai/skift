package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/ivy/dagger/internal/store"
)

// Completion carries the result: a markdown body under the size cap, plus
// the artifacts the work produced. Results and artifacts of a node's
// blocking predecessors are what a downstream agent assembles its context
// from, which is what makes graph resume possible. See Completion and
// results, and Artifact, in docs/domain-model.md.

// Errors a complete_node is rejected with. A body over its ceiling is a
// [*LimitError]; see limits.go. Its artifacts are rejected as an
// attach_artifact would reject them; see artifact.go.
var (
	// ErrResultRequired: a completion carries a result body, since the
	// result is what downstream reads.
	ErrResultRequired = errors.New("a completion needs a result body")
)

// CompleteNode moves an open node to done with the result it produced.
// The node must be open. A held node is fenced like any other write: the
// patch must carry the node's current lease, so a worker whose lease was
// reclaimed cannot land a late result.
type CompleteNode struct {
	// Node is a node reference.
	Node string `json:"node"`
	// Result is what the work produced.
	Result NodeResult `json:"result"`
}

// NodeResult is what completing a node produces: a markdown summary and
// zero or more artifacts.
type NodeResult struct {
	// Body is markdown under [Limits.ResultBytes]. Required.
	Body string `json:"body"`
	// Artifacts are outputs beyond text, attached to the node on
	// completion.
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// validate rejects a completion that cannot be applied however the graph
// stands: no body, a body over its ceiling, an invalid artifact, or two
// artifacts of one name.
func (c *CompleteNode) validate(l Limits) error {
	r := c.Result
	if r.Body == "" {
		return ErrResultRequired
	}
	if err := exceeded(LimitResultBytes, int64(len(r.Body)), int64(l.ResultBytes)); err != nil {
		return err
	}
	names := map[string]bool{}
	for i := range r.Artifacts {
		a := &r.Artifacts[i]
		if err := a.validate(l); err != nil {
			return fmt.Errorf("artifact %d: %w", i, err)
		}
		if names[a.Name] {
			return fmt.Errorf("artifact %q: named twice in one result", a.Name)
		}
		names[a.Name] = true
	}
	return nil
}

// Completed is what a complete_node event records as its after: the node
// as done, the result row, and the artifacts attached with it. The result
// travels on the event so the log reconstructs what the node produced,
// not only that it did.
type Completed struct {
	Node      store.Node       `json:"node"`
	Result    store.Result     `json:"result"`
	Artifacts []StoredArtifact `json:"artifacts"`
}

func (a *applier) completeNode(ctx context.Context, op *CompleteNode) error {
	resolved, err := a.resolver.Resolve(ctx, op.Node)
	if err != nil {
		return err
	}
	if err := a.fence(ctx, resolved.ID); err != nil {
		return err
	}
	before, err := a.tx.GetNode(ctx, store.GetNodeParams{OrgID: a.orgID, ID: resolved.ID})
	if err != nil {
		return fmt.Errorf("read node %s: %w", op.Node, err)
	}
	if before.Status != "open" {
		return fmt.Errorf("node %s is %s: %w", op.Node, before.Status, ErrStatusConflict)
	}
	if err := a.uniqueArtifactNames(ctx, before.ID, op.Result.Artifacts); err != nil {
		return err
	}

	after, err := a.tx.SetNodeStatus(ctx, store.SetNodeStatusParams{OrgID: a.orgID, ID: before.ID, Status: "done"})
	if err != nil {
		return fmt.Errorf("%s %s: %w", OpCompleteNode, op.Node, err)
	}
	result, err := a.tx.CreateResult(ctx, store.CreateResultParams{
		ID: newULID(), OrgID: a.orgID, NodeID: after.ID, Body: op.Result.Body, CompletedBy: a.actor.PrincipalID,
	})
	if err != nil {
		return fmt.Errorf("record result of %s: %w", op.Node, err)
	}
	artifacts := make([]StoredArtifact, 0, len(op.Result.Artifacts))
	for i := range op.Result.Artifacts {
		stored, err := a.attach(ctx, after.ID, &op.Result.Artifacts[i])
		if err != nil {
			return err
		}
		artifacts = append(artifacts, stored)
	}
	a.record(after)
	if err := a.event(ctx, OpCompleteNode, after.ID, after.ProjectID, nil, before, Completed{Node: after, Result: result, Artifacts: artifacts}); err != nil {
		return err
	}
	// The attempt is over, so its lease ends here rather than at expiry.
	return a.releaseLease(ctx, after, ReleaseComplete)
}
