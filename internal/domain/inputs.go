package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

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
