package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/store"
)

// Input is one blocking predecessor of a node as a downstream agent reads
// it: the node, its current result, and its artifacts. Each carries its
// author, because a predecessor's result is a claim by that principal
// rather than a fact. See Content is data, not instructions, in
// docs/domain-model.md.
type Input struct {
	Node store.Node `json:"node"`
	// Result is the predecessor's current result, or nil when it has
	// none: it is open, or was dropped rather than completed.
	Result *store.Result `json:"result"`
	// Artifacts are every artifact attached to the predecessor, in
	// attachment order.
	Artifacts []StoredArtifact `json:"artifacts"`
}

// Inputs returns the results and artifacts of the node's blocking
// predecessors: the source of every blocks edge into it, ordered by
// project then number. A predecessor that has not completed is returned
// with no result, so the consumer sees what it is waiting on as well as
// what it has.
func Inputs(ctx context.Context, tx store.Tx, orgID, nodeID string) ([]Input, error) {
	if _, err := tx.GetNode(ctx, store.GetNodeParams{OrgID: orgID, ID: nodeID}); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeNotFound
	} else if err != nil {
		return nil, fmt.Errorf("read node %s: %w", nodeID, err)
	}
	rows, err := tx.Tx.Query(ctx, blockingPredecessors, orgID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("read predecessors of node %s: %w", nodeID, err)
	}
	predecessors, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.Node])
	if err != nil {
		return nil, fmt.Errorf("read predecessors of node %s: %w", nodeID, err)
	}
	inputs := make([]Input, 0, len(predecessors))
	for _, p := range predecessors {
		in := Input{Node: p, Artifacts: []StoredArtifact{}}
		results, err := tx.ListResultsForNode(ctx, store.ListResultsForNodeParams{OrgID: orgID, NodeID: p.ID})
		if err != nil {
			return nil, fmt.Errorf("read results of node %s: %w", p.ID, err)
		}
		if p.Status == "done" && len(results) > 0 {
			in.Result = &results[0]
		}
		in.Artifacts, err = readArtifacts(ctx, tx, orgID, p.ID)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, in)
	}
	return inputs, nil
}

// blockingPredecessors are the sources of blocks edges into node $2 of
// organization $1, whatever their status.
const blockingPredecessors = `
SELECT b.*
FROM edges e
JOIN nodes b ON b.org_id = e.org_id AND b.id = e.from_node
WHERE e.org_id = $1 AND e.to_node = $2 AND e.kind = 'blocks'
ORDER BY b.project_id, b.number`

// OpReadInputs is the action reading a node's inputs is authorized as. It
// is a read on the node whose inputs are asked for, so it writes no event
// and audits no denial; the default bundle lets every principal of an
// organization read within it, and a deployment that narrows reads does so
// in policy, as it does for search.
const OpReadInputs = "read_inputs"

// InputState is one input as a client reads it: the predecessor with its
// derived state, which is where its reference comes from, the result it
// completed with, and its artifacts.
type InputState struct {
	NodeState
	// Result is the predecessor's current result, or nil when it has
	// none: it was dropped rather than completed.
	Result *StoredResult
	// Artifacts are every artifact attached to the predecessor, in
	// attachment order.
	Artifacts []StoredArtifact
}

// InputsHint is how much a node's inputs come to without carrying them.
// Two integers are enough for a consumer to decide whether to ask for the
// bodies; see the token cost of returning content unasked in
// AGENT_REVIEW.md.
type InputsHint struct {
	// Count is how many of the node's satisfied predecessors have a
	// result.
	Count int
	// Bytes is the total size of those result bodies.
	Bytes int64
}

// ReadInputs returns the predecessors that satisfy the node ref names,
// with their results and artifacts. project is what a bare `#7` resolves
// in, and may be empty when ref carries its own.
//
// Only a predecessor that satisfies the node is an input. An open one
// satisfies nothing and is left out: it is what the node is waiting on,
// which Explain answers. A dropped one is included with no result,
// because a drop satisfies the dependency without producing anything.
// Inputs are not transitive; a predecessor's result is the summary of its
// own inputs.
func ReadInputs(ctx context.Context, tx store.Tx, pol *policy.Bundle, orgID string, actor Actor, project, ref string) ([]InputState, error) {
	if err := actor.validate(); err != nil {
		return nil, err
	}
	resolved, err := resolveNode(ctx, tx, orgID, project, ref)
	if err != nil {
		return nil, err
	}
	node, err := tx.GetNode(ctx, store.GetNodeParams{OrgID: orgID, ID: resolved.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeNotFound
	} else if err != nil {
		return nil, fmt.Errorf("read node %s: %w", resolved.ID, err)
	}
	r := &recorder{tx: tx, orgID: orgID, actor: actor, policy: pol}
	if err := r.authorizeRead(ctx, OpReadInputs, node); err != nil {
		return nil, err
	}
	all, err := Inputs(ctx, tx, orgID, node.ID)
	if err != nil {
		return nil, err
	}
	satisfying := make([]Input, 0, len(all))
	nodes := make([]store.Node, 0, len(all))
	for _, in := range all {
		if in.Node.Status == "open" {
			continue
		}
		satisfying = append(satisfying, in)
		nodes = append(nodes, in.Node)
	}
	states, err := annotate(ctx, tx, orgID, nodes)
	if err != nil {
		return nil, err
	}
	out := make([]InputState, 0, len(satisfying))
	for i, in := range satisfying {
		out = append(out, InputState{NodeState: states[i], Result: in.Result, Artifacts: in.Artifacts})
	}
	return out, nil
}

// InputsSize is the hint a response carries beside a node: how many of its
// satisfied predecessors have a result and what those results come to. It
// reads the sizes rather than the bodies, so carrying the hint costs
// nothing the caller would notice.
func InputsSize(ctx context.Context, tx store.Tx, orgID, nodeID string) (InputsHint, error) {
	var hint InputsHint
	row := tx.Tx.QueryRow(ctx, inputsSize, orgID, nodeID)
	if err := row.Scan(&hint.Count, &hint.Bytes); err != nil {
		return InputsHint{}, fmt.Errorf("size inputs of node %s: %w", nodeID, err)
	}
	return hint, nil
}

// inputsSize counts the current results of the blocking predecessors of
// node $2 of organization $1 and adds up their bodies. Only a done
// predecessor has a current result, which is the newest of its rows.
const inputsSize = `
SELECT count(*)::int AS count, coalesce(sum(octet_length(r.body)), 0)::bigint AS bytes
FROM edges e
JOIN nodes b ON b.org_id = e.org_id AND b.id = e.from_node
JOIN LATERAL (
    SELECT body FROM results
    WHERE org_id = b.org_id AND node_id = b.id
    ORDER BY created_at DESC, id DESC
    LIMIT 1
) r ON true
WHERE e.org_id = $1 AND e.to_node = $2 AND e.kind = 'blocks' AND b.status = 'done'`
