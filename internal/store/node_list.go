package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// NodeFilter narrows a listing of nodes. Its zero value narrows nothing.
type NodeFilter struct {
	// Projects keeps nodes of any of these project ids. Empty means every
	// project of the organization.
	Projects []string
	// Status keeps nodes in one stored status: "open", "done" or "dropped".
	Status string
	// Kinds keeps nodes of any of these kinds.
	Kinds []string
	// AssigneeKind is "", "agent" or "user", as [ReadyFilter.AssigneeKind].
	AssigneeKind string
	// Labels keeps nodes carrying every one of them, and AnyLabels nodes
	// carrying at least one.
	Labels    []string
	AnyLabels []string
	// MinPriority keeps nodes at this priority or more urgent.
	MinPriority string
	// Held keeps only the nodes a hold parks, on the node, on something
	// containing it, or on its project.
	Held bool
	// ParentID keeps the children of one node.
	ParentID string
}

// FilterNodes returns one page of the nodes of organization orgID that f
// keeps, ordered by project then by number so a page is stable across any
// number of projects.
func (t Tx) FilterNodes(ctx context.Context, orgID string, f NodeFilter, limit, offset int) ([]Node, error) {
	sql, args, err := nodeListSQL(orgID, f, limit, offset)
	if err != nil {
		return nil, err
	}
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Node])
}

// nodeListSQL renders the listing. $1 is always the organization, so a
// filter can narrow the page and never widen it.
func nodeListSQL(orgID string, f NodeFilter, limit, offset int) (string, []any, error) {
	var b strings.Builder
	b.WriteString("SELECT n.* FROM nodes n WHERE n.org_id = $1")
	args := []any{orgID}
	where := func(clause string, value any) {
		args = append(args, value)
		b.WriteString(" AND ")
		fmt.Fprintf(&b, clause, len(args))
	}
	if len(f.Projects) > 0 {
		where("n.project_id = ANY($%d::text[])", f.Projects)
	}
	switch f.Status {
	case "":
	case "open", "done", "dropped":
		where("n.status = $%d", f.Status)
	default:
		return "", nil, fmt.Errorf("%w: status %q is not open, done, or dropped", ErrInvalidFilter, f.Status)
	}
	if len(f.Kinds) > 0 {
		where("n.kind = ANY($%d::text[])", f.Kinds)
	}
	switch f.AssigneeKind {
	case "":
	case "agent":
		b.WriteString(" AND (n.assignee_kind IS NULL OR n.assignee_kind = 'agent')")
	case "user":
		b.WriteString(" AND n.assignee_kind = 'user'")
	default:
		return "", nil, fmt.Errorf("%w: assignee kind %q is not agent or user", ErrInvalidFilter, f.AssigneeKind)
	}
	if len(f.Labels) > 0 {
		where("n.labels @> $%d::text[]", f.Labels)
	}
	if len(f.AnyLabels) > 0 {
		where("n.labels && $%d::text[]", f.AnyLabels)
	}
	if f.MinPriority != "" {
		where("n.priority <= $%d", f.MinPriority)
	}
	if f.Held {
		b.WriteString(" AND " + heldWhere)
	}
	if f.ParentID != "" {
		where("n.parent_id = $%d", f.ParentID)
	}
	b.WriteString(" ORDER BY n.project_id, n.number")
	args = append(args, limit, offset)
	fmt.Fprintf(&b, " LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	return b.String(), args, nil
}
