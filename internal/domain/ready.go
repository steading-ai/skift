package domain

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/steading-ai/skift/internal/store"
)

// ErrOneProject is returned by [Ready] when the query does not name exactly
// one project. Priority is comparable only within a project, so an ordered
// ready set across several would be arithmetic on incomparable scales.
var ErrOneProject = errors.New("an ordered ready query names exactly one project")

// ErrInvalidFilter is wrapped by every refusal of a ready or list filter
// value no node can have: a status, assignee kind or priority outside its
// vocabulary. It is the store's own value, so a refusal from either layer
// matches it.
var ErrInvalidFilter = store.ErrInvalidFilter

// ErrNodeNotFound is returned by [Explain] when the organization has no such
// node.
var ErrNodeNotFound = errors.New("node not found")

// Assignee kinds, matching Principal.kind. See the Node section of
// docs/domain-model.md.
const (
	// AssigneeAgent marks a node as agent work. A node with no assignee
	// kind is agent-eligible too, so a filter on this kind includes it.
	AssigneeAgent = "agent"
	// AssigneeUser makes a node a gate: only completion by a user satisfies
	// the edges out of it.
	AssigneeUser = "user"
)

// ReadyQuery is the caller's filters over the ready set. The zero value
// matches every ready node in the organization.
type ReadyQuery struct {
	// Projects restricts the set to these project ids. Empty means every
	// project of the organization. An ordered query ([Ready]) names
	// exactly one; [ReadySet] and the round-robin selection functions take
	// any number, including zero.
	Projects []string
	// Kinds restricts the set to nodes of any of these kinds (OR). Empty
	// means every kind.
	Kinds []string
	// AssigneeKind restricts the set to nodes completable by that kind of
	// principal: [AssigneeUser] selects gates; [AssigneeAgent] selects
	// agent-assigned nodes and nodes with no assignee kind, which are
	// agent-eligible. Empty means any.
	AssigneeKind string
	// Labels restricts the set to nodes carrying every one of them (AND).
	Labels []string
	// AnyLabels restricts the set to nodes carrying at least one of them
	// (OR). Empty means labels do not narrow this way.
	AnyLabels []string
	// MinPriority restricts the set to nodes at this priority or more
	// urgent, `p0` through `p4`. Empty means any.
	MinPriority string
	// Parent restricts the set to the children of one node, as a node
	// reference. Empty means every node. A bare `#7` resolves within the
	// query's single project when it names exactly one, and otherwise
	// must qualify its own.
	Parent string
	// Limit caps the number of rows returned, at most [Limits.PageSize].
	// Zero takes that ceiling.
	Limit int
	// Offset skips that many rows of the ordered set, for the next page.
	// Only an ordered query pages.
	Offset int
}

// Ready returns the ready nodes of one project that match q, most urgent
// and then oldest first: priority, created_at, id. The outer scan is
// nodes_open_by_project_idx, which is in that order. q must name exactly
// one project, or [ErrOneProject] is returned.
func Ready(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery) ([]store.Node, error) {
	if len(q.Projects) != 1 {
		return nil, ErrOneProject
	}
	if q.Offset < 0 {
		return nil, errors.New("offset is negative")
	}
	parentID, err := resolveParent(ctx, tx, orgID, q)
	if err != nil {
		return nil, err
	}
	return queryReady(ctx, tx, orgID, q, true, parentID)
}

// ReadySet returns the ready nodes matching q across any number of
// projects, in no meaningful order. It exists for callers that want the
// set rather than a queue: reporting, and the from-scratch recomputation a
// property test compares against.
func ReadySet(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery) ([]store.Node, error) {
	parentID, err := resolveParent(ctx, tx, orgID, q)
	if err != nil {
		return nil, err
	}
	return queryReady(ctx, tx, orgID, q, false, parentID)
}

func queryReady(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery, ordered bool, parentID string) ([]store.Node, error) {
	f, err := q.filter(parentID)
	if err != nil {
		return nil, err
	}
	limit, err := boundPageSize(q.Limit, ActiveLimits())
	if err != nil {
		return nil, err
	}
	return tx.ReadyNodes(ctx, orgID, q.Projects, f, store.ReadyPage{Ordered: ordered, Limit: limit, Offset: q.Offset})
}

// resolveParent resolves q.Parent to a node id, or returns "" when it is
// empty. A bare `#7` resolves within q's project when it names exactly
// one; otherwise the reference must qualify its own project, the same
// rule [resolveProject] applies everywhere else a project is optional.
func resolveParent(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery) (string, error) {
	if q.Parent == "" {
		return "", nil
	}
	var project *store.Project
	if len(q.Projects) == 1 {
		p, err := tx.GetProject(ctx, store.GetProjectParams{OrgID: orgID, ID: q.Projects[0]})
		if err != nil {
			return "", fmt.Errorf("read project %s: %w", q.Projects[0], err)
		}
		project = &p
	}
	resolved, err := NewResolver(tx, orgID, project).Resolve(ctx, q.Parent)
	if err != nil {
		return "", err
	}
	return resolved.ID, nil
}

// filter is q's non-project filters as the store applies them, or why one
// of them is not valid.
func (q ReadyQuery) filter(parentID string) (store.ReadyFilter, error) {
	switch q.AssigneeKind {
	case "", AssigneeAgent, AssigneeUser:
	default:
		return store.ReadyFilter{}, fmt.Errorf("%w: assignee kind %q is not agent or user", ErrInvalidFilter, q.AssigneeKind)
	}
	if q.MinPriority != "" && !validPriority(q.MinPriority) {
		return store.ReadyFilter{}, fmt.Errorf("%w: priority %q is not p0 through p4", ErrInvalidFilter, q.MinPriority)
	}
	return store.ReadyFilter{
		Kinds: q.Kinds, AssigneeKind: q.AssigneeKind, Labels: q.Labels, AnyLabels: q.AnyLabels,
		MinPriority: q.MinPriority, ParentID: parentID,
	}, nil
}

// eligibleProjects returns, in no particular order, the ids of the
// projects among q.Projects (every project of the organization when
// empty) that hold at least one node matching q's other filters and
// ready. It is the unlocked project-selection step [claimOnce] and
// [Service.Ready] use before running the existing single-project ordered
// query against the project or projects the rotation picks; it never
// itself locks a row, so it never conflicts with `FOR UPDATE`.
func eligibleProjects(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery, parentID string) ([]string, error) {
	f, err := q.filter(parentID)
	if err != nil {
		return nil, err
	}
	return tx.EligibleProjects(ctx, orgID, q.Projects, f)
}

// nextProject picks which of eligible to draw from next, given the
// project chosen last, so a rotation cursor need only remember one value.
// last nil, empty, or no longer eligible starts the cycle from the
// beginning. eligible must not be empty.
func nextProject(last *string, eligible []string) string {
	return rotationOrder(last, eligible)[0]
}

// rotationOrder is every project of eligible, visited once, in turn order
// starting from whichever [nextProject](last, eligible) would pick —
// the same project a claim would draw from next. eligible is sorted
// ascending by id first: project ids are ULIDs, so this is also creation
// order, and it needs no extra lookup since every candidate row already
// carries it. It lets a caller that wants more than one turn's worth read
// every eligible project's share in a single pass before any of them
// repeats. eligible must not be empty.
func rotationOrder(last *string, eligible []string) []string {
	sorted := append([]string(nil), eligible...)
	sort.Strings(sorted)
	i := 0
	if last != nil {
		for j, id := range sorted {
			if id == *last {
				i = (j + 1) % len(sorted)
				break
			}
		}
	}
	order := make([]string, 0, len(sorted))
	order = append(order, sorted[i:]...)
	order = append(order, sorted[:i]...)
	return order
}

// resolveScope resolves refs, the projects a request named explicitly, to
// their ids, refusing an archived one by name exactly as [resolveProject]
// does for a single project: naming a project explicitly still means
// naming it, even among several. Empty refs resolves to nil, meaning
// every project of the organization.
func resolveScope(ctx context.Context, tx store.Tx, orgID string, refs []string) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		p, err := resolveProject(ctx, tx, orgID, ref)
		if err != nil {
			return nil, err
		}
		if p.ArchivedAt != nil {
			return nil, fmt.Errorf("project %s: %w", p.Slug, ErrProjectArchived)
		}
		ids = append(ids, p.ID)
	}
	return ids, nil
}

func validPriority(p string) bool {
	switch p {
	case "p0", "p1", "p2", "p3", "p4":
		return true
	}
	return false
}

// ReasonKind is one condition of readiness that does not hold.
type ReasonKind string

const (
	// ReasonNotOpen: the node is done or dropped.
	ReasonNotOpen ReasonKind = "not_open"
	// ReasonArchived: the node's project is archived and read-only.
	ReasonArchived ReasonKind = "archived"
	// ReasonBlocked: a predecessor has not satisfied its `blocks` edge.
	ReasonBlocked ReasonKind = "blocked"
	// ReasonLeased: a live lease holds the node.
	ReasonLeased ReasonKind = "leased"
	// ReasonOpenChild: an open child keeps the node from being ready.
	ReasonOpenChild ReasonKind = "open_child"
	// ReasonHeld: the node, or a node containing it, carries a hold.
	ReasonHeld ReasonKind = "held"
	// ReasonProjectHeld: the node's project carries a hold.
	ReasonProjectHeld ReasonKind = "project_held"
)

// Hold is the stored decision that keeps work out of the ready set: which
// node carries it, who applied it, when, and why. A hold is never
// permanent, so a held node is parked rather than stuck.
type Hold struct {
	// Number and Title identify the node carrying the hold.
	Number int64
	Title  string
	// Self reports that the hold is on the node being explained rather
	// than on something containing it.
	Self bool
	At   time.Time
	By   string
	// Reason is why the work was parked. Always set: a hold nobody can
	// explain is one nobody dares release.
	Reason string
	// Group is the focus sweep that placed the hold, if a sweep did.
	Group *string
}

// Reason is one structured answer to why a node is not ready. Kind says
// which of the optional fields is set.
type Reason struct {
	Kind ReasonKind
	// Status is the node's stored status, for [ReasonNotOpen].
	Status string
	// Blocker is the predecessor, for [ReasonBlocked].
	Blocker *store.Node
	// Gate reports that Blocker is user-assigned, so only its completion
	// by a user satisfies the edge: the node is waiting on a person.
	Gate bool
	// Lease is the live lease, with its holder and expiry, for
	// [ReasonLeased].
	Lease *store.Lease
	// Child is the open child, for [ReasonOpenChild].
	Child *store.Node
	// Project is the project, for [ReasonArchived] and
	// [ReasonProjectHeld].
	Project *store.Project
	// Hold is the hold keeping the node parked, for [ReasonHeld].
	Hold *Hold
	// Permanent reports that no transition of the graph can clear this
	// reason: a dropped gate, since dropped is terminal and a gate is
	// satisfied only by completion; or the node itself being dropped.
	Permanent bool
}

func (r Reason) String() string {
	switch r.Kind {
	case ReasonNotOpen:
		return "it is " + r.Status
	case ReasonArchived:
		return "its project " + r.Project.Slug + " is archived"
	case ReasonBlocked:
		what := "open node"
		if r.Gate {
			what = r.Blocker.Status + " gate"
		}
		return fmt.Sprintf("it is blocked by %s #%d", what, r.Blocker.Number)
	case ReasonLeased:
		return fmt.Sprintf("it is held by lease %s until %s", r.Lease.ID, r.Lease.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"))
	case ReasonOpenChild:
		return fmt.Sprintf("its child #%d is open", r.Child.Number)
	case ReasonHeld:
		where := fmt.Sprintf("it is contained in #%d, which is", r.Hold.Number)
		if r.Hold.Self {
			where = "it is"
		}
		return fmt.Sprintf("%s held since %s: %s", where,
			r.Hold.At.UTC().Format("2006-01-02T15:04:05Z"), r.Hold.Reason)
	case ReasonProjectHeld:
		return fmt.Sprintf("its project %s is held: %s", r.Project.Slug, *r.Project.HeldReason)
	}
	return string(r.Kind)
}

// Explanation is why one node is not ready. Reasons is empty when it is.
type Explanation struct {
	Node    store.Node
	Reasons []Reason
}

// Ready reports whether every readiness condition holds.
func (e *Explanation) Ready() bool {
	return len(e.Reasons) == 0
}

// Stuck reports a node with no possible path to readiness: it is open, and
// something nothing can clear keeps it out of the ready set. That is always
// a bug rather than a state, which is why it is worth detecting on its own.
// See the "Why not ready" section of docs/domain-model.md.
func (e *Explanation) Stuck() bool {
	if e.Node.Status != "open" {
		return false
	}
	for _, r := range e.Reasons {
		if r.Permanent {
			return true
		}
	}
	return false
}

// Explain answers why the node is not ready, as the same conditions Ready
// checks, un-negated. Reasons come in a fixed order: not open, archived,
// the project's hold, the holds on the node and what contains it, then
// blockers by project then number, the live lease, open children by
// number. Project-scoped facts come before node-scoped ones.
func Explain(ctx context.Context, tx store.Tx, orgID, nodeID string) (*Explanation, error) {
	node, err := tx.GetNode(ctx, store.GetNodeParams{OrgID: orgID, ID: nodeID})
	if errors.Is(err, store.ErrNoRows) {
		return nil, ErrNodeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("explain node: %w", err)
	}
	e := &Explanation{Node: node}

	if node.Status != "open" {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonNotOpen, Status: node.Status, Permanent: node.Status == "dropped"})
	}

	projects, err := tx.ArchivedProjectOf(ctx, orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonArchived, Project: &projects[i]})
	}

	heldProjects, err := tx.HeldProjectOf(ctx, orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range heldProjects {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonProjectHeld, Project: &heldProjects[i]})
	}

	// Every hold in the chain, not only the nearest: the node becomes
	// ready when all of them are released, so reporting one would leave
	// an item off the list.
	holds, err := tx.HeldAncestorsOf(ctx, orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for _, h := range holds {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonHeld, Hold: &Hold{
			Number: h.Number,
			Title:  h.Title,
			Self:   h.Self,
			At:     h.HeldAt,
			By:     h.HeldBy,
			Reason: h.HeldReason,
			Group:  h.HoldGroup,
		}})
	}

	blockers, err := tx.UnsatisfiedBlockersOf(ctx, orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range blockers {
		b := &blockers[i]
		gate := b.AssigneeKind != nil && *b.AssigneeKind == AssigneeUser
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonBlocked, Blocker: b, Gate: gate, Permanent: gate && b.Status == "dropped"})
	}

	leases, err := tx.LiveLeaseOf(ctx, orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range leases {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonLeased, Lease: &leases[i]})
	}

	children, err := tx.OpenChildrenOf(ctx, orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range children {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonOpenChild, Child: &children[i]})
	}
	return e, nil
}
