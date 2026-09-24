package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steading-ai/skift/internal/store"
)

// Operation names for holds, as written to events.op.
const (
	OpHoldNode      = "hold_node"
	OpUnholdNode    = "unhold_node"
	OpHoldProject   = "hold_project"
	OpUnholdProject = "unhold_project"
)

// ErrNotHeld is returned when releasing something that carries no hold.
var ErrNotHeld = errors.New("carries no hold")

// ErrAmbiguousFocus is returned when a scope has more than one sweep with
// live holds, so clearing "the" sweep would have to guess which. Naming
// the group resolves it.
var ErrAmbiguousFocus = errors.New("more than one focus sweep is active in this scope")

// HoldNode parks a node. Containment carries the hold, so everything under
// it is parked too and nothing is written to those descendants.
type HoldNode struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// UnholdNode releases a node's own hold. A hold on something containing it
// is untouched, and keeps the node parked until that one is released too.
type UnholdNode struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

func (a *applier) holdNode(ctx context.Context, op *HoldNode) error {
	return a.changeHold(ctx, OpHoldNode, op.Node, op.Reason, func(ctx context.Context, before store.Node) (store.Node, error) {
		if before.HeldAt != nil {
			return store.Node{}, fmt.Errorf("node %s is already held: %w", op.Node, ErrStatusConflict)
		}
		return a.tx.HoldNode(ctx, store.HoldNodeParams{
			OrgID: a.orgID, ID: before.ID, HeldBy: &a.actor.PrincipalID, HeldReason: &op.Reason,
		})
	})
}

func (a *applier) unholdNode(ctx context.Context, op *UnholdNode) error {
	return a.changeHold(ctx, OpUnholdNode, op.Node, op.Reason, func(ctx context.Context, before store.Node) (store.Node, error) {
		if before.HeldAt == nil {
			return store.Node{}, fmt.Errorf("node %s %w", op.Node, ErrNotHeld)
		}
		return a.tx.UnholdNode(ctx, store.UnholdNodeParams{OrgID: a.orgID, ID: before.ID})
	})
}

// changeHold applies one node's hold change and records it with its
// reason. Holding is not a status change, so the node's status is not
// touched, and containment carries the hold at read time, so no
// descendant is written.
func (a *applier) changeHold(ctx context.Context, opName, ref, reason string, change func(context.Context, store.Node) (store.Node, error)) error {
	resolved, err := a.resolver.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	if err := a.fence(ctx, resolved.ID); err != nil {
		return err
	}
	before, err := a.tx.GetNode(ctx, store.GetNodeParams{OrgID: a.orgID, ID: resolved.ID})
	if err != nil {
		return fmt.Errorf("read node %s: %w", ref, err)
	}
	after, err := change(ctx, before)
	if err != nil {
		return err
	}
	a.record(after)
	return a.event(ctx, opName, after.ID, after.ProjectID, &reason, before, after)
}

// heldWhere is the effectively-held predicate on n, with $1 the
// organization: the same two conditions Ready negates, un-negated. It is
// composed from those fragments rather than written again beside them, so
// a condition cannot be added to one and forgotten in the other. The flat
// project probe comes first, so a held project answers for every one of
// its nodes without instantiating the walk.
const heldWhere = `(EXISTS (` + heldProject + `) OR EXISTS (` + heldAncestors + `))`

// HeldCount is how many open nodes of a project are held rather than
// ready. It answers what an empty ready page owes its caller: whether the
// work is finished or parked.
func HeldCount(ctx context.Context, tx store.Tx, orgID, projectID string) (int, error) {
	const sql = `SELECT count(*) FROM nodes n
WHERE n.org_id = $1 AND n.project_id = $2 AND n.status = 'open' AND ` + heldWhere
	var count int
	if err := tx.Tx.QueryRow(ctx, sql, orgID, projectID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count held nodes: %w", err)
	}
	return count, nil
}

// FocusResult reports what one sweep did.
type FocusResult struct {
	// Group is the sweep's id, recorded on every hold it placed.
	Group string
	// Held is how many projects or nodes the sweep parked.
	Held int
	// Released is how many holds the sweep it replaced gave back.
	Released int
}

// HoldProject parks a project: every node in it leaves the ready set and
// none of their statuses change. Unlike archiving, the project stays
// writable, so parked work can still be planned. Held is how many open
// nodes left the ready set.
func (g *Service) HoldProject(ctx context.Context, orgID string, actor Actor, requestID, ref, reason string) (*Project, int, error) {
	var held int
	project, err := g.projectHold(ctx, orgID, actor, requestID, ref, reason, OpHoldProject,
		func(ctx context.Context, r *recorder, before store.Project) (*store.Project, error) {
			if before.HeldAt != nil {
				return &before, nil
			}
			after, err := r.tx.HoldProject(ctx, store.HoldProjectParams{
				OrgID: orgID, ID: before.ID, HeldBy: &actor.PrincipalID, HeldReason: &reason,
			})
			if err != nil {
				return nil, fmt.Errorf("hold project %s: %w", before.Slug, err)
			}
			if held, err = HeldCount(ctx, r.tx, orgID, before.ID); err != nil {
				return nil, err
			}
			return &after, r.projectEvent(ctx, OpHoldProject, &reason, before, after)
		})
	return project, held, err
}

// UnholdProject releases a project's hold, returning its work to the ready
// set unless something else holds it back.
func (g *Service) UnholdProject(ctx context.Context, orgID string, actor Actor, requestID, ref, reason string) (*Project, error) {
	return g.projectHold(ctx, orgID, actor, requestID, ref, reason, OpUnholdProject,
		func(ctx context.Context, r *recorder, before store.Project) (*store.Project, error) {
			if before.HeldAt == nil {
				return &before, nil
			}
			after, err := r.tx.UnholdProject(ctx, store.UnholdProjectParams{OrgID: orgID, ID: before.ID})
			if err != nil {
				return nil, fmt.Errorf("unhold project %s: %w", before.Slug, err)
			}
			return &after, r.projectEvent(ctx, OpUnholdProject, &reason, before, after)
		})
}

// projectHold runs one project-level hold change under its request id,
// idempotently: holding a held project changes nothing and records
// nothing, the way archiving an archived one does.
func (g *Service) projectHold(ctx context.Context, orgID string, actor Actor, requestID, ref, reason, op string,
	change func(context.Context, *recorder, store.Project) (*store.Project, error),
) (*Project, error) {
	if err := validHoldRequest(requestID, actor, reason, op); err != nil {
		return nil, err
	}
	if ref == "" {
		return nil, ErrProjectRequired
	}
	hash, err := requestHash(struct {
		Op      string `json:"op"`
		Project string `json:"project"`
		Reason  string `json:"reason"`
	}{op, ref, reason})
	if err != nil {
		return nil, err
	}
	return idempotent(ctx, g.s, orgID, actor, requestID, hash, func(ctx context.Context, tx store.Tx) (*store.Project, error) {
		before, err := resolveProject(ctx, tx, orgID, ref)
		if err != nil {
			return nil, err
		}
		return change(ctx, g.recorder(tx, orgID, actor, &requestID), *before)
	})
}

// validHoldRequest checks what every hold change owes before it touches
// the graph: an idempotency key, an actor the transport resolved, and the
// reason the event log keeps.
func validHoldRequest(requestID string, actor Actor, reason, op string) error {
	if requestID == "" {
		return ErrNoRequestID
	}
	if err := actor.validate(); err != nil {
		return err
	}
	if reason == "" {
		return fmt.Errorf("%s: %w", op, ErrReasonRequired)
	}
	return nil
}

// Focus parks everything outside a keep-set. With no project it sweeps the
// organization's projects; with one it sweeps that project's open nodes.
//
// It is declarative and replacing: the sweep already active in the scope
// is released in the same transaction, so at most one is ever live and
// [Service.ClearFocus] never has to guess which. It holds only what is not
// already held, so a hold placed on its own is neither adopted by the
// sweep nor released when the sweep is cleared.
func (g *Service) Focus(ctx context.Context, orgID string, actor Actor, requestID, projectRef string, keep []string, reason string) (FocusResult, error) {
	if err := validHoldRequest(requestID, actor, reason, OpHoldNode); err != nil {
		return FocusResult{}, err
	}
	if err := exceeded(LimitKeepSet, int64(len(keep)), int64(ActiveLimits().KeepSet)); err != nil {
		return FocusResult{}, err
	}
	hash, err := requestHash(struct {
		Op      string   `json:"op"`
		Project string   `json:"project"`
		Keep    []string `json:"keep"`
		Reason  string   `json:"reason"`
	}{"focus", projectRef, keep, reason})
	if err != nil {
		return FocusResult{}, err
	}

	result, err := idempotent(ctx, g.s, orgID, actor, requestID, hash, func(ctx context.Context, tx store.Tx) (*FocusResult, error) {
		r := g.recorder(tx, orgID, actor, &requestID)
		scope, err := focusScope(ctx, tx, orgID, projectRef)
		if err != nil {
			return nil, err
		}
		released, err := clearSweep(ctx, r, scope, "", reason)
		if err != nil {
			return nil, err
		}
		group := newULID()
		held, err := sweep(ctx, r, scope, keep, reason, group)
		if err != nil {
			return nil, err
		}
		return &FocusResult{Group: group, Held: held, Released: released}, nil
	})
	if err != nil {
		return FocusResult{}, err
	}
	return *result, nil
}

// ClearFocus releases the holds one sweep placed. group is optional: a
// scope has at most one active sweep, so the ordinary call names none.
func (g *Service) ClearFocus(ctx context.Context, orgID string, actor Actor, requestID, projectRef, group, reason string) (int, error) {
	if err := validHoldRequest(requestID, actor, reason, OpUnholdNode); err != nil {
		return 0, err
	}
	hash, err := requestHash(struct {
		Op      string `json:"op"`
		Project string `json:"project"`
		Group   string `json:"group"`
		Reason  string `json:"reason"`
	}{"clear_focus", projectRef, group, reason})
	if err != nil {
		return 0, err
	}
	released, err := idempotent(ctx, g.s, orgID, actor, requestID, hash, func(ctx context.Context, tx store.Tx) (*int, error) {
		scope, err := focusScope(ctx, tx, orgID, projectRef)
		if err != nil {
			return nil, err
		}
		n, err := clearSweep(ctx, g.recorder(tx, orgID, actor, &requestID), scope, group, reason)
		return &n, err
	})
	if err != nil {
		return 0, err
	}
	return *released, nil
}

// focus is the scope one sweep covers: an organization, whose keep-set
// names projects, or a single project, whose keep-set names its nodes.
// Project is nil for the organization.
type focus struct {
	Project *store.Project
}

// focusScope resolves the scope a sweep was asked for. An empty reference
// is the organization.
func focusScope(ctx context.Context, tx store.Tx, orgID, projectRef string) (focus, error) {
	if projectRef == "" {
		return focus{}, nil
	}
	project, err := resolveProject(ctx, tx, orgID, projectRef)
	if err != nil {
		return focus{}, err
	}
	return focus{Project: project}, nil
}

// clearSweep releases the sweep active in the scope, or the one named,
// and returns how many holds came back. A scope with no sweep is a no-op
// rather than a mistake; one with two and no group named is ambiguous
// rather than a guess.
func clearSweep(ctx context.Context, r *recorder, scope focus, group, reason string) (int, error) {
	chosen, err := activeSweep(ctx, r, scope, group)
	if err != nil || chosen == "" {
		return 0, err
	}
	if scope.Project == nil {
		rows, err := r.tx.ReleaseProjectHoldGroup(ctx, store.ReleaseProjectHoldGroupParams{OrgID: r.orgID, HoldGroup: &chosen})
		if err != nil {
			return 0, fmt.Errorf("clear focus: %w", err)
		}
		for _, after := range rows {
			if err := r.projectEvent(ctx, OpUnholdProject, &reason, projectUnheld(after, chosen), after); err != nil {
				return 0, err
			}
		}
		return len(rows), nil
	}
	rows, err := r.tx.ReleaseNodeHoldGroup(ctx, store.ReleaseNodeHoldGroupParams{OrgID: r.orgID, HoldGroup: &chosen})
	if err != nil {
		return 0, fmt.Errorf("clear focus: %w", err)
	}
	for _, after := range rows {
		if err := r.event(ctx, OpUnholdNode, after.ID, after.ProjectID, &reason, nodeUnheld(after, chosen), after); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// activeSweep is the sweep to act on: the one named, or the scope's only
// live one. Empty means the scope carries no sweep.
func activeSweep(ctx context.Context, r *recorder, scope focus, named string) (string, error) {
	if named != "" {
		return named, nil
	}
	var groups []string
	var err error
	if scope.Project == nil {
		groups, err = r.tx.ProjectHoldGroups(ctx, r.orgID)
	} else {
		groups, err = r.tx.NodeHoldGroups(ctx, store.NodeHoldGroupsParams{OrgID: r.orgID, ProjectID: scope.Project.ID})
	}
	if err != nil {
		return "", fmt.Errorf("find focus sweeps: %w", err)
	}
	switch len(groups) {
	case 0:
		return "", nil
	case 1:
		return groups[0], nil
	}
	return "", fmt.Errorf("%w: %s", ErrAmbiguousFocus, strings.Join(groups, ", "))
}

// sweep parks everything in the scope that the keep-set does not keep
// live, marking each hold with the sweep's group.
func sweep(ctx context.Context, r *recorder, scope focus, keep []string, reason, group string) (int, error) {
	if scope.Project == nil {
		return sweepProjects(ctx, r, keep, reason, group)
	}
	return sweepNodes(ctx, r, scope.Project, keep, reason, group)
}

// sweepProjects holds every live project outside the keep-set. An
// archived project is already out of the ready set and is left alone.
func sweepProjects(ctx context.Context, r *recorder, keep []string, reason, group string) (int, error) {
	ids := make([]string, 0, len(keep))
	for _, ref := range keep {
		p, err := resolveProject(ctx, r.tx, r.orgID, ref)
		if err != nil {
			return 0, err
		}
		ids = append(ids, p.ID)
	}
	rows, err := r.tx.HoldProjectsOutside(ctx, store.HoldProjectsOutsideParams{
		OrgID: r.orgID, HeldBy: &r.actor.PrincipalID, HeldReason: &reason, HoldGroup: &group, Keep: ids,
	})
	if err != nil {
		return 0, fmt.Errorf("focus: %w", err)
	}
	// One sweep writes one event per hold, so it answers to the same
	// ceiling a patch of that many operations would. The transaction
	// rolls back, so the refusal holds nothing.
	if err := exceeded(LimitOpsPerPatch, int64(len(rows)), int64(ActiveLimits().OpsPerPatch)); err != nil {
		return 0, err
	}
	for _, after := range rows {
		if err := r.projectEvent(ctx, OpHoldProject, &reason, projectUnheld(after, ""), after); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// sweepNodes holds the open nodes of one project that the keep-set does
// not keep live.
//
// A kept node keeps its whole lineage live in both directions. Its
// descendants are the work it names, and its ancestors have to stay live
// too, because holding a node parks everything it contains: parking the
// parent of a kept epic would park the epic.
//
// Only the topmost node outside that live set is held. Containment
// carries a hold, so holding a node under one already held would write a
// second fact saying what the first already says.
func sweepNodes(ctx context.Context, r *recorder, project *store.Project, keep []string, reason, group string) (int, error) {
	resolver := NewResolver(r.tx, r.orgID, project)
	kept := map[string]bool{}
	for _, ref := range keep {
		resolved, err := resolver.Resolve(ctx, ref)
		if err != nil {
			return 0, err
		}
		if resolved.ProjectID != project.ID {
			return 0, fmt.Errorf("%s is not in project %s: a sweep keeps the nodes of the project it is scoped to", ref, project.Slug)
		}
		kept[resolved.ID] = true
	}

	nodes, err := r.tx.OpenNodesOfProject(ctx, store.OpenNodesOfProjectParams{OrgID: r.orgID, ProjectID: project.ID})
	if err != nil {
		return 0, fmt.Errorf("focus: %w", err)
	}
	parent := map[string]string{}
	for _, n := range nodes {
		if n.ParentID != nil {
			parent[n.ID] = *n.ParentID
		}
	}

	// A node is live when a kept node is somewhere on its path to the
	// root: at or below one keeps it as work the keep-set named, above
	// one keeps it because parking it would park what is kept. One walk
	// answers both, marking the whole chain it climbed.
	live := map[string]bool{}
	for _, n := range nodes {
		chain := ancestry(parent, n.ID)
		for _, at := range chain {
			if kept[at] {
				for _, on := range chain {
					live[on] = true
				}
				break
			}
		}
	}

	var ids []string
	for _, n := range nodes {
		if live[n.ID] {
			continue
		}
		if at, ok := parent[n.ID]; ok && !live[at] {
			// An ancestor outside the live set is held instead, and
			// containment carries that hold down to this node.
			continue
		}
		ids = append(ids, n.ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// One sweep writes one event per hold, so it answers to the same
	// ceiling a patch of that many operations would.
	if err := exceeded(LimitOpsPerPatch, int64(len(ids)), int64(ActiveLimits().OpsPerPatch)); err != nil {
		return 0, err
	}

	rows, err := r.tx.HoldNodeSet(ctx, store.HoldNodeSetParams{
		OrgID: r.orgID, HeldBy: &r.actor.PrincipalID, HeldReason: &reason, HoldGroup: &group, Ids: ids,
	})
	if err != nil {
		return 0, fmt.Errorf("focus: %w", err)
	}
	for _, after := range rows {
		if err := r.event(ctx, OpHoldNode, after.ID, after.ProjectID, &reason, nodeUnheld(after, ""), after); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// ancestry is id and every node containing it, nearest first. The parent
// relation is acyclic, which CheckAcyclic guarantees, so the walk ends.
func ancestry(parent map[string]string, id string) []string {
	chain := []string{id}
	for at, ok := parent[id]; ok; at, ok = parent[at] {
		chain = append(chain, at)
	}
	return chain
}

// nodeUnheld and projectUnheld are the row as it stood before a hold
// change, for the event's before image. group is the sweep releasing it,
// or empty when the change is a hold being placed.
func nodeUnheld(after store.Node, group string) store.Node {
	before := after
	before.HeldAt, before.HeldBy, before.HeldReason, before.HoldGroup = nil, nil, nil, nil
	if group != "" {
		before = after
		before.HoldGroup = &group
	}
	return before
}

func projectUnheld(after store.Project, group string) store.Project {
	before := after
	before.HeldAt, before.HeldBy, before.HeldReason, before.HoldGroup = nil, nil, nil, nil
	if group != "" {
		before = after
		before.HoldGroup = &group
	}
	return before
}
