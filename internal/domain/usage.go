package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ivy/dagger/internal/store"
)

// Usage is the append-only record of what work cost, in token counts and
// never currency: a rate table is applied at query time by whoever wants
// money. An actual and an estimate are rows of one shape with different
// kinds, and both carry the principal that wrote them. An actual is
// attributed to the lease as well as the node, so a retry is a second
// attempt with its own rows rather than more tokens on the first. An
// estimate has an author and a history: re-estimating adds a row, and the
// node's current estimate is the most recent one, derived on read. Subtree
// rollups are queries over the parent relation, computed on every read.
// See the Usage section of docs/domain-model.md.
//
// Reporting an actual is a write to the node's attempt, so it carries the
// lease like every other write to a held node. An estimate changes
// neither the node's row nor its readiness, so it is not fenced; whether
// a principal may estimate a node at all is the policy's question, asked
// of the node.

// Usage kinds, as the kind column names them.
const (
	UsageActual   = "actual"
	UsageEstimate = "estimate"
)

// Operation names, as written to events.op.
const (
	OpReportUsage  = "report_usage"
	OpEstimateNode = "estimate_node"
)

// Errors a usage operation is rejected with.
var (
	// ErrNegativeTokens: a count below zero.
	ErrNegativeTokens = errors.New("token counts are not negative")
	// ErrNoTokens: a record of nothing says nothing, and would count as
	// an attempt or an estimate all the same.
	ErrNoTokens = errors.New("a usage record needs at least one token count")
	// ErrBasisRequired: an estimate says what it was reasoned from, or it
	// is a number nobody can revisit.
	ErrBasisRequired = errors.New("an estimate needs a basis")
	// ErrUsageNeedsLease: an actual is attributed to an attempt, and the
	// patch names none.
	ErrUsageNeedsLease = errors.New("an actual is attributed to a lease, and the patch carries none")
)

// Tokens are the counts a usage record carries. Counts, never currency.
type Tokens struct {
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

func (t Tokens) validate() error {
	if t.InputTokens < 0 || t.OutputTokens < 0 || t.CacheReadTokens < 0 || t.CacheWriteTokens < 0 {
		return ErrNegativeTokens
	}
	if t.InputTokens == 0 && t.OutputTokens == 0 && t.CacheReadTokens == 0 && t.CacheWriteTokens == 0 {
		return ErrNoTokens
	}
	return nil
}

// ReportUsage records what an attempt on a node cost so far. The patch
// must carry the lease of that attempt, which is the node's live lease;
// the actor reporting is the lease's holder. Reporting renews the lease
// as any write by the holder does.
type ReportUsage struct {
	// Node is a node reference: the node the carried lease holds.
	Node string `json:"node"`
	// Model is a free string naming the model. Optional.
	Model string `json:"model,omitempty"`
	Tokens
}

// EstimateNode records what a node is expected to cost, and why. It needs
// no lease: an estimate precedes any attempt. The prior estimate, if any,
// is kept; the node's current estimate becomes this one.
type EstimateNode struct {
	// Node is a node reference.
	Node string `json:"node"`
	// Model is a free string naming the model the estimate assumes.
	// Optional.
	Model string `json:"model,omitempty"`
	Tokens
	// Basis is what the estimate was reasoned from. Required.
	Basis string `json:"basis"`
}

func (e *EstimateNode) validate() error {
	if e.Basis == "" {
		return ErrBasisRequired
	}
	return e.Tokens.validate()
}

// ModelTotals is one group of a rollup: every record of one model summed.
type ModelTotals struct {
	// Model is the model the records named, or nil for records naming
	// none.
	Model *string `json:"model"`
	// Records is how many rows the totals sum. For actuals, one per
	// report; for estimates, one per node with a current estimate.
	Records int64 `json:"records"`
	Tokens
}

// Rollup is what a subtree cost and what it is expected to cost, grouped
// by model. Actual sums every actual on the root and its descendants;
// Estimate sums the current estimate of each of those nodes, so an
// estimate superseded by a later one counts for nothing. Both are
// computed when asked for; nothing stores a total.
type Rollup struct {
	Actual   []ModelTotals `json:"actual"`
	Estimate []ModelTotals `json:"estimate"`
}

// ListUsage returns every usage record of a node, oldest first: actuals
// and estimates alike, each attributed to its principal and, for an
// actual, its lease.
func ListUsage(ctx context.Context, tx store.Tx, orgID, nodeID string) ([]store.Usage, error) {
	if err := nodeExists(ctx, tx, orgID, nodeID); err != nil {
		return nil, err
	}
	rows, err := tx.ListAllUsageForNode(ctx, store.ListAllUsageForNodeParams{OrgID: orgID, NodeID: nodeID})
	if err != nil {
		return nil, fmt.Errorf("read usage of node %s: %w", nodeID, err)
	}
	return rows, nil
}

// CurrentEstimate is a node's most recent estimate, or nil when it has
// none.
func CurrentEstimate(ctx context.Context, tx store.Tx, orgID, nodeID string) (*store.Usage, error) {
	if err := nodeExists(ctx, tx, orgID, nodeID); err != nil {
		return nil, err
	}
	u, err := tx.GetCurrentEstimate(ctx, store.GetCurrentEstimateParams{OrgID: orgID, NodeID: nodeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read estimate of node %s: %w", nodeID, err)
	}
	return &u, nil
}

// SubtreeRollup computes the [Rollup] of a node and every descendant
// through the parent relation.
func SubtreeRollup(ctx context.Context, tx store.Tx, orgID, rootID string) (*Rollup, error) {
	if err := nodeExists(ctx, tx, orgID, rootID); err != nil {
		return nil, err
	}
	actuals, err := tx.RollupActuals(ctx, store.RollupActualsParams{OrgID: orgID, ID: rootID})
	if err != nil {
		return nil, fmt.Errorf("roll up actuals under node %s: %w", rootID, err)
	}
	estimates, err := tx.RollupEstimates(ctx, store.RollupEstimatesParams{OrgID: orgID, ID: rootID})
	if err != nil {
		return nil, fmt.Errorf("roll up estimates under node %s: %w", rootID, err)
	}
	r := &Rollup{Actual: make([]ModelTotals, 0, len(actuals)), Estimate: make([]ModelTotals, 0, len(estimates))}
	for _, a := range actuals {
		r.Actual = append(r.Actual, ModelTotals{Model: a.Model, Records: a.Records, Tokens: Tokens{
			InputTokens: a.InputTokens, OutputTokens: a.OutputTokens, CacheReadTokens: a.CacheReadTokens, CacheWriteTokens: a.CacheWriteTokens,
		}})
	}
	for _, e := range estimates {
		r.Estimate = append(r.Estimate, ModelTotals{Model: e.Model, Records: e.Records, Tokens: Tokens{
			InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CacheReadTokens: e.CacheReadTokens, CacheWriteTokens: e.CacheWriteTokens,
		}})
	}
	return r, nil
}

// nodeExists is [ErrNodeNotFound] for an id naming no node in the
// organization, so a read of an unknown node is not an empty answer.
func nodeExists(ctx context.Context, tx store.Tx, orgID, nodeID string) error {
	_, err := tx.GetNode(ctx, store.GetNodeParams{OrgID: orgID, ID: nodeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNodeNotFound
	}
	if err != nil {
		return fmt.Errorf("read node %s: %w", nodeID, err)
	}
	return nil
}

func (a *applier) reportUsage(ctx context.Context, op *ReportUsage) error {
	node, err := a.resolver.Resolve(ctx, op.Node)
	if err != nil {
		return err
	}
	if a.lease == nil {
		return fmt.Errorf("node %s: %w", op.Node, ErrUsageNeedsLease)
	}
	if a.lease.NodeID != node.ID {
		return fmt.Errorf("node %s: %w", op.Node, ErrLeaseMismatch)
	}
	if err := a.fence(ctx, node.ID); err != nil {
		return err
	}
	return a.recordUsage(ctx, node, store.CreateUsageParams{
		ID: newULID(), OrgID: a.orgID, NodeID: node.ID, LeaseID: &a.lease.ID, Principal: a.actor.PrincipalID,
		Model: optional(op.Model), Kind: UsageActual,
		InputTokens: op.InputTokens, OutputTokens: op.OutputTokens,
		CacheReadTokens: op.CacheReadTokens, CacheWriteTokens: op.CacheWriteTokens,
	}, OpReportUsage)
}

func (a *applier) estimateNode(ctx context.Context, op *EstimateNode) error {
	node, err := a.resolver.Resolve(ctx, op.Node)
	if err != nil {
		return err
	}
	return a.recordUsage(ctx, node, store.CreateUsageParams{
		ID: newULID(), OrgID: a.orgID, NodeID: node.ID, Principal: a.actor.PrincipalID,
		Model: optional(op.Model), Kind: UsageEstimate, Basis: &op.Basis,
		InputTokens: op.InputTokens, OutputTokens: op.OutputTokens,
		CacheReadTokens: op.CacheReadTokens, CacheWriteTokens: op.CacheWriteTokens,
	}, OpEstimateNode)
}

// recordUsage appends one usage row and the event recording it.
func (a *applier) recordUsage(ctx context.Context, node Resolved, params store.CreateUsageParams, op string) error {
	u, err := a.tx.CreateUsage(ctx, params)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	a.usage = append(a.usage, u)
	return a.event(ctx, op, node.ID, node.ProjectID, nil, nil, u)
}
