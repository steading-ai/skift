package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ivy/dagger/internal/store"
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
	// Ready: open, not blocked, no live lease, no open children, and its
	// project is not archived.
	Ready bool
	// Claimed: a live lease holds the node.
	Claimed bool
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

// derivedRow is one row of derivedSQL.
type derivedRow struct {
	ID           string `db:"id"`
	Number       int64  `db:"number"`
	Slug         string `db:"slug"`
	Status       string `db:"status"`
	Blocked      bool   `db:"blocked"`
	Claimed      bool   `db:"claimed"`
	OpenChild    bool   `db:"open_child"`
	Archived     bool   `db:"archived"`
	HumanTouched bool   `db:"human_touched"`
}

// derivedSQL computes every flag of a set of nodes in one statement, from
// the readiness fragments Ready and Explain share. $1 is the organization,
// $2 the node ids.
const derivedSQL = `
SELECT n.id, n.number, pr.slug, n.status,
       EXISTS (` + unsatisfiedBlockers + `) AS blocked,
       EXISTS (` + liveLease + `) AS claimed,
       EXISTS (` + openChildren + `) AS open_child,
       EXISTS (` + archivedProject + `) AS archived,
       EXISTS (SELECT 1 FROM events ev
               WHERE ev.org_id = $1 AND ev.node_id = n.id AND ev.principal_kind = 'user') AS human_touched
FROM nodes n
JOIN projects pr ON pr.org_id = $1 AND pr.id = n.project_id
WHERE n.org_id = $1 AND n.id = ANY($2::text[])`

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
	rows, err := tx.Tx.Query(ctx, derivedSQL, orgID, ids)
	if err != nil {
		return nil, fmt.Errorf("derive node state: %w", err)
	}
	derived, err := pgx.CollectRows(rows, pgx.RowToStructByName[derivedRow])
	if err != nil {
		return nil, fmt.Errorf("derive node state: %w", err)
	}
	for _, d := range derived {
		out[d.ID] = Derived{
			Blocked:      d.Blocked,
			Ready:        d.Status == "open" && !d.Blocked && !d.Claimed && !d.OpenChild && !d.Archived,
			Claimed:      d.Claimed,
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
	if errors.Is(err, pgx.ErrNoRows) {
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

	leases, err := explainRows[store.Lease](ctx, tx, liveLease, "", orgID, nodeID)
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
	return v, nil
}

// NodeListQuery is one page of a project's nodes, filtered. Project is
// required; every other filter is optional and narrows the page.
type NodeListQuery struct {
	// Project names the project by slug, `org/slug`, or id.
	Project string
	// Status keeps nodes in one stored status: open, done, or dropped.
	Status string
	// Kind keeps nodes of one kind.
	Kind string
	// AssigneeKind keeps nodes completable by that kind of principal, as
	// [ReadyQuery.AssigneeKind] does.
	AssigneeKind string
	// Labels keeps nodes carrying every one of them.
	Labels []string
	// Parent keeps the children of one node, as a node reference.
	Parent string
	// Limit caps the page, at most [Limits.PageSize]. Zero takes that
	// ceiling.
	Limit int
	// Offset skips that many nodes, for the next page.
	Offset int
}

// ListNodes returns one page of the project's nodes matching q, by
// number.
func (g *Service) ListNodes(ctx context.Context, orgID string, q NodeListQuery) ([]NodeState, error) {
	if q.Project == "" {
		return nil, ErrProjectRequired
	}
	if q.Offset < 0 {
		return nil, errors.New("offset is negative")
	}
	var out []NodeState
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		project, err := resolveProject(ctx, tx, orgID, q.Project)
		if err != nil {
			return err
		}
		var parentID string
		if q.Parent != "" {
			parent, err := NewResolver(tx, orgID, project).Resolve(ctx, q.Parent)
			if err != nil {
				return err
			}
			parentID = parent.ID
		}
		sql, args, err := q.sql(orgID, project.ID, parentID)
		if err != nil {
			return err
		}
		rows, err := tx.Tx.Query(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("list nodes: %w", err)
		}
		nodes, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.Node])
		if err != nil {
			return fmt.Errorf("list nodes: %w", err)
		}
		out, err = annotate(ctx, tx, orgID, nodes)
		return err
	})
	return out, err
}

// sql renders the listing. $1 is always the organization and $2 the
// project, so a filter can narrow the page and never widen it.
func (q NodeListQuery) sql(orgID, projectID, parentID string) (string, []any, error) {
	var b strings.Builder
	b.WriteString("SELECT n.* FROM nodes n WHERE n.org_id = $1 AND n.project_id = $2")
	args := []any{orgID, projectID}
	where := func(clause string, value any) {
		args = append(args, value)
		b.WriteString(" AND ")
		fmt.Fprintf(&b, clause, len(args))
	}
	switch q.Status {
	case "":
	case "open", "done", "dropped":
		where("n.status = $%d", q.Status)
	default:
		return "", nil, fmt.Errorf("status %q is not open, done, or dropped", q.Status)
	}
	if q.Kind != "" {
		where("n.kind = $%d", q.Kind)
	}
	switch q.AssigneeKind {
	case "":
	case AssigneeAgent:
		b.WriteString(" AND (n.assignee_kind IS NULL OR n.assignee_kind = 'agent')")
	case AssigneeUser:
		b.WriteString(" AND n.assignee_kind = 'user'")
	default:
		return "", nil, fmt.Errorf("assignee kind %q is not agent or user", q.AssigneeKind)
	}
	if len(q.Labels) > 0 {
		where("n.labels @> $%d::text[]", q.Labels)
	}
	if parentID != "" {
		where("n.parent_id = $%d", parentID)
	}
	b.WriteString(" ORDER BY n.number")
	limit, err := boundPageSize(q.Limit, ActiveLimits())
	if err != nil {
		return "", nil, err
	}
	args = append(args, limit, q.Offset)
	fmt.Fprintf(&b, " LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	return b.String(), args, nil
}
