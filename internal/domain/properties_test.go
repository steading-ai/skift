package domain_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// The invariants the system rests on, stated as properties over random
// histories rather than as examples. See docs/adrs/007-core-tech-stack.md.
//
// A history is a sequence of steps against a fresh organization: patches
// the way a client writes them, plus the lease and completion moves that
// change readiness without going through Apply. Every step is drawn from
// the state the previous ones left, so the generator only ever proposes
// references that exist; whether a step is accepted is the domain's call.
// After each step the invariants are checked against an independent
// recomputation in Go from the raw rows.

// propertyChecks is how many histories each property runs, unless
// -rapid.checks was given on the command line.
const propertyChecks = "1000"

// history is one random run. It tracks what the generator needs to draw
// the next step and keeps a readable log of every step, which is what a
// failure reports after shrinking.
type history struct {
	ctx     context.Context
	t       *rapid.T
	s       *store.Store
	org     string
	project string
	user    domain.Actor
	agent   domain.Actor
	// nodes is the highest number committed to the project: `#1` through
	// `#nodes` exist.
	nodes int64
	// leases is every lease a claim produced, live or not.
	leases []store.Lease
	seq    int
	log    []string
}

// The generator's alphabet of step kinds, drawn by weight.
const (
	stepPatch = iota
	stepComplete
	stepClaim
	stepRelease
	stepExpire
)

// step draws and performs one step of the history.
func (h *history) step() {
	t := h.t
	var kind int
	switch roll := rapid.IntRange(0, 99).Draw(t, "step"); {
	case roll < 60:
		kind = stepPatch
	case roll < 72:
		kind = stepComplete
	case roll < 84:
		kind = stepClaim
	case roll < 92:
		kind = stepRelease
	default:
		kind = stepExpire
	}
	switch {
	case kind == stepComplete && h.nodes > 0:
		h.complete()
	case kind == stepClaim && h.nodes > 0:
		h.claim()
	case kind == stepRelease && len(h.unreleased()) > 0:
		h.release()
	case kind == stepExpire && len(h.unreleased()) > 0:
		h.expire()
	default:
		h.patch()
	}
}

// patch draws one patch, applies it as the user, and checks that a
// rejected patch left the graph byte-identical.
func (h *history) patch() {
	t := h.t
	h.seq++
	p := domain.Patch{RequestID: fmt.Sprintf("req-%d", h.seq), Project: "platform"}
	var aliases []string
	for range rapid.IntRange(1, 4).Draw(t, "ops") {
		p.Ops = append(p.Ops, h.drawOp(&aliases))
	}

	before := h.snapshot()
	r, err := domain.Apply(h.ctx, h.s, pol, h.org, h.user, p)
	if err != nil {
		h.logf("%s\n    => rejected: %v", renderPatch(p), err)
		if after := h.snapshot(); after != before {
			h.fatalf("a rejected patch changed the graph\nbefore: %s\nafter:  %s", before, after)
		}
		return
	}
	for _, n := range r.Nodes {
		h.nodes = max(h.nodes, n.Number)
	}
	h.logf("%s\n    => accepted", renderPatch(p))
}

// drawOp draws one operation over the nodes that exist and the aliases
// created earlier in the same patch. With nothing to refer to, only a
// create is possible.
func (h *history) drawOp(aliases *[]string) domain.Op {
	t := h.t
	refs := h.refs(*aliases)
	kind := "create"
	if len(refs) > 0 {
		kind = rapid.SampledFrom([]string{"create", "link", "unlink", "drop", "reopen", "reparent", "hold", "unhold"}).Draw(t, "op")
	}
	ref := func(label string) string { return rapid.SampledFrom(refs).Draw(t, label) }

	switch kind {
	case "link":
		return domain.Op{AddEdge: &domain.AddEdge{From: ref("from"), To: ref("to")}}
	case "unlink":
		return domain.Op{RemoveEdge: &domain.RemoveEdge{From: ref("from"), To: ref("to")}}
	case "drop":
		reason := rapid.SampledFrom([]string{domain.DropSuperseded, domain.DropNotViable, domain.DropNoLongerRelevant, domain.DropOther}).Draw(t, "reason")
		op := &domain.DropNode{Node: ref("node"), Reason: reason}
		if rapid.Bool().Draw(t, "cascade") {
			// A bound the generated graphs never reach, so the
			// cascade itself is what is exercised rather than its
			// refusal.
			op.Cascade = &domain.Cascade{MaxNodes: 1000}
		}
		if reason == domain.DropOther {
			op.Detail = "because"
		}
		return domain.Op{DropNode: op}
	case "reopen":
		return domain.Op{ReopenNode: &domain.ReopenNode{Node: ref("node"), Reason: "again"}}
	case "hold":
		return domain.Op{HoldNode: &domain.HoldNode{Node: ref("node"), Reason: "parked"}}
	case "unhold":
		return domain.Op{UnholdNode: &domain.UnholdNode{Node: ref("node"), Reason: "back"}}
	case "reparent":
		parent := ""
		if rapid.IntRange(0, 3).Draw(t, "root") > 0 {
			parent = ref("parent")
		}
		return domain.Op{UpdateNode: &domain.UpdateNode{Node: ref("node"), Parent: &parent}}
	}

	alias := fmt.Sprintf("n%d", len(*aliases))
	op := &domain.CreateNode{Alias: alias, Title: alias}
	op.AssigneeKind = rapid.SampledFrom([]string{"", domain.AssigneeAgent, domain.AssigneeUser}).Draw(t, "assignee_kind")
	op.Priority = rapid.SampledFrom([]string{"p0", "p1", "p2", "p3", "p4"}).Draw(t, "priority")
	if len(refs) > 0 {
		if rapid.IntRange(0, 2).Draw(t, "under") == 0 {
			op.Parent = ref("parent")
		}
		for range rapid.IntRange(0, 2).Draw(t, "blockers") {
			op.BlockedBy = append(op.BlockedBy, ref("blocked_by"))
		}
	}
	*aliases = append(*aliases, alias)
	return domain.Op{CreateNode: op}
}

// refs is every reference the next operation may use: each committed
// node by number, and each alias the patch has declared so far.
func (h *history) refs(aliases []string) []string {
	refs := make([]string, 0, int(h.nodes)+len(aliases))
	for n := int64(1); n <= h.nodes; n++ {
		refs = append(refs, fmt.Sprintf("#%d", n))
	}
	return append(refs, aliases...)
}

// complete marks a node done. complete_node is not an operation Apply
// knows yet, so the status is written the way that operation will write
// it, without a patch.
func (h *history) complete() {
	number := rapid.Int64Range(1, h.nodes).Draw(h.t, "complete")
	tag, err := h.s.Pool().Exec(h.ctx,
		`UPDATE nodes SET status = 'done' WHERE org_id = $1 AND project_id = $2 AND number = $3`,
		h.org, h.project, number)
	if err != nil {
		h.fatalf("complete #%d: %v", number, err)
	}
	h.logf("complete #%d => %d row", number, tag.RowsAffected())
}

// claim takes the next ready agent-eligible node for the agent, if any
// is ready, the way an orchestrator asks for work.
func (h *history) claim() {
	h.seq++
	req := domain.ClaimRequest{
		RequestID: fmt.Sprintf("req-%d", h.seq), Projects: []string{"platform"}, AssigneeKind: domain.AssigneeAgent,
		Harness: "spec", AgentRef: "spec",
	}
	c, err := domain.Claim(h.ctx, h.s, pol, h.org, h.agent, req)
	var denied *domain.Denial
	switch {
	case errors.As(err, &denied):
		h.logf("claim => denied: %v", err)
	case err != nil:
		h.fatalf("claim: %v", err)
	case c == nil:
		h.logf("claim => nothing ready")
	default:
		h.leases = append(h.leases, c.Lease)
		h.logf("claim => #%d held by lease %s", c.Node.Number, c.Lease.ID)
	}
}

// release ends one of the agent's leases with a reason. A lease this
// history expired is not live any more, and the release is refused.
func (h *history) release() {
	h.seq++
	id := rapid.SampledFrom(h.unreleased()).Draw(h.t, "release")
	reason := rapid.SampledFrom([]string{
		domain.ReleaseComplete, domain.ReleaseGate, domain.ReleaseCrash, domain.ReleaseBudget, domain.ReleasePreempted,
	}).Draw(h.t, "reason")
	_, err := domain.Release(h.ctx, h.s, pol, h.org, h.agent, fmt.Sprintf("req-%d", h.seq), id, reason, "")
	switch {
	case errors.Is(err, domain.ErrLeaseNotLive):
		h.logf("release %s as %s => rejected: %v", id, reason, err)
	case err != nil:
		h.fatalf("release %s: %v", id, err)
	default:
		h.markReleased(id)
		h.logf("release %s as %s", id, reason)
	}
}

// expire moves a lease's clock back so its TTL has passed, the way time
// does. The schema keeps expiry after the claim, so both move.
func (h *history) expire() {
	id := rapid.SampledFrom(h.unreleased()).Draw(h.t, "expire")
	_, err := h.s.Pool().Exec(h.ctx,
		`UPDATE leases SET claimed_at = now() - interval '2 minutes', expires_at = now() - interval '1 minute'
		 WHERE org_id = $1 AND id = $2`, h.org, id)
	if err != nil {
		h.fatalf("expire %s: %v", id, err)
	}
	h.logf("expire %s", id)
}

// unreleased is the id of every lease no release step has ended. An
// expired lease stays here: releasing it is a rejection worth exercising.
func (h *history) unreleased() []string {
	var out []string
	for _, l := range h.leases {
		if l.ReleasedAt == nil {
			out = append(out, l.ID)
		}
	}
	return out
}

func (h *history) markReleased(id string) {
	now := time.Now()
	for i := range h.leases {
		if h.leases[i].ID == id {
			h.leases[i].ReleasedAt = &now
		}
	}
}

// snapshot is the whole graph of the organization as PostgreSQL renders
// it: every row of every table the wait relation and readiness read, in a
// fixed order. Two snapshots are equal only if the rows are identical.
func (h *history) snapshot() string {
	const sql = `
SELECT json_build_object(
  'projects', (SELECT coalesce(json_agg(p ORDER BY p.id), '[]'::json) FROM projects p WHERE p.org_id = $1),
  'nodes',    (SELECT coalesce(json_agg(n ORDER BY n.id), '[]'::json) FROM nodes n WHERE n.org_id = $1),
  'edges',    (SELECT coalesce(json_agg(e ORDER BY e.from_node, e.to_node, e.kind), '[]'::json) FROM edges e WHERE e.org_id = $1),
  'leases',   (SELECT coalesce(json_agg(l ORDER BY l.id), '[]'::json) FROM leases l WHERE l.org_id = $1)
)::text`
	var out string
	if err := h.s.Pool().QueryRow(h.ctx, sql, h.org).Scan(&out); err != nil {
		h.fatalf("snapshot: %v", err)
	}
	return out
}

func (h *history) logf(format string, args ...any) {
	h.log = append(h.log, fmt.Sprintf(format, args...))
}

// fatalf fails the property with the whole history so far, so the shrunk
// counterexample reads as the sequence of steps that produced it.
func (h *history) fatalf(format string, args ...any) {
	var b strings.Builder
	fmt.Fprintf(&b, format, args...)
	b.WriteString("\n\nhistory:")
	for i, line := range h.log {
		fmt.Fprintf(&b, "\n%3d. %s", i+1, line)
	}
	h.t.Fatalf("%s", b.String())
}

// renderPatch writes a patch the way a person would read it.
func renderPatch(p domain.Patch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "patch %s:", p.RequestID)
	for _, op := range p.Ops {
		b.WriteString("\n      ")
		b.WriteString(renderOp(op))
	}
	return b.String()
}

func renderOp(op domain.Op) string {
	switch {
	case op.CreateNode != nil:
		c := op.CreateNode
		s := fmt.Sprintf("create %s %s", c.Alias, c.Priority)
		if c.AssigneeKind != "" {
			s += " " + c.AssigneeKind
		}
		if c.Parent != "" {
			s += " under " + c.Parent
		}
		if len(c.BlockedBy) > 0 {
			s += " blocked_by " + strings.Join(c.BlockedBy, ",")
		}
		return s
	case op.UpdateNode != nil:
		u := op.UpdateNode
		if u.Parent != nil && *u.Parent == "" {
			return fmt.Sprintf("reparent %s to root", u.Node)
		}
		return fmt.Sprintf("reparent %s under %s", u.Node, *u.Parent)
	case op.DropNode != nil:
		d := op.DropNode
		s := fmt.Sprintf("drop %s as %s", d.Node, d.Reason)
		if d.Cascade != nil {
			s += fmt.Sprintf(" cascade max_nodes %d", d.Cascade.MaxNodes)
		}
		return s
	case op.ReopenNode != nil:
		return "reopen " + op.ReopenNode.Node
	case op.AddEdge != nil:
		return fmt.Sprintf("link %s -> %s", op.AddEdge.From, op.AddEdge.To)
	case op.RemoveEdge != nil:
		return fmt.Sprintf("unlink %s -> %s", op.RemoveEdge.From, op.RemoveEdge.To)
	}
	return "(empty)"
}

// state is the raw rows the invariants are recomputed from.
type state struct {
	nodes    []store.Node
	edges    []store.Edge
	leases   []store.Lease
	projects []store.Project
	// ready is what the domain's ready query returned over the same
	// snapshot, unordered, and ordered over the project.
	readySet     []store.Node
	readyOrdered []store.Node
}

// load reads every row of the organization and the domain's ready view in
// one transaction, so the recomputation and the view see the same graph.
func (h *history) load() *state {
	st := &state{}
	err := h.s.WithTx(h.ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if st.nodes, err = queryRows[store.Node](ctx, tx, `SELECT * FROM nodes WHERE org_id = $1 ORDER BY id`, h.org); err != nil {
			return err
		}
		if st.edges, err = queryRows[store.Edge](ctx, tx, `SELECT * FROM edges WHERE org_id = $1 ORDER BY from_node, to_node, kind`, h.org); err != nil {
			return err
		}
		if st.leases, err = queryRows[store.Lease](ctx, tx, `SELECT * FROM leases WHERE org_id = $1 ORDER BY id`, h.org); err != nil {
			return err
		}
		if st.projects, err = queryRows[store.Project](ctx, tx, `SELECT * FROM projects WHERE org_id = $1 ORDER BY id`, h.org); err != nil {
			return err
		}
		if st.readySet, err = domain.ReadySet(ctx, tx, h.org, domain.ReadyQuery{}); err != nil {
			return err
		}
		st.readyOrdered, err = domain.Ready(ctx, tx, h.org, domain.ReadyQuery{Projects: []string{h.project}})
		return err
	})
	if err != nil {
		h.fatalf("load state: %v", err)
	}
	return st
}

func queryRows[T any](ctx context.Context, tx store.Tx, sql string, args ...any) ([]T, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[T])
}

// waits is the combined wait relation as a map from each node to the
// nodes it waits for: the sources of `blocks` edges into it, and its
// children. See the Acyclicity section of docs/domain-model.md.
func (st *state) waits() map[string][]string {
	waits := map[string][]string{}
	for _, e := range st.edges {
		if e.Kind == "blocks" {
			waits[e.ToNode] = append(waits[e.ToNode], e.FromNode)
		}
	}
	for _, n := range st.nodes {
		if n.ParentID != nil {
			waits[*n.ParentID] = append(waits[*n.ParentID], n.ID)
		}
	}
	return waits
}

// findCycle walks the wait relation depth-first from every node and
// returns the first cycle it closes, as node ids in wait order, or nil.
func (st *state) findCycle() []string {
	waits := st.waits()
	const (
		unseen = iota
		active
		finished
	)
	color := map[string]int{}
	var path []string
	var walk func(id string) []string
	walk = func(id string) []string {
		color[id] = active
		path = append(path, id)
		for _, next := range waits[id] {
			switch color[next] {
			case active:
				return append(slices.Clone(path[slices.Index(path, next):]), next)
			case unseen:
				if cycle := walk(next); cycle != nil {
					return cycle
				}
			}
		}
		path = path[:len(path)-1]
		color[id] = finished
		return nil
	}
	for _, n := range st.nodes {
		if color[n.ID] == unseen {
			if cycle := walk(n.ID); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// recomputeReady derives the ready set from the rows alone, by the
// definition in the Readiness section of docs/domain-model.md: open, in a
// live project, every `blocks` predecessor satisfied, no live lease, no
// hold on it or on anything containing it, and no open child.
func (st *state) recomputeReady(now time.Time) []store.Node {
	byID := map[string]store.Node{}
	for _, n := range st.nodes {
		byID[n.ID] = n
	}
	archived := map[string]bool{}
	for _, p := range st.projects {
		archived[p.ID] = p.ArchivedAt != nil
	}
	leased := map[string]bool{}
	for _, l := range st.leases {
		if l.ReleasedAt == nil && l.ExpiresAt.After(now) {
			leased[l.NodeID] = true
		}
	}
	projectHeld := map[string]bool{}
	for _, p := range st.projects {
		projectHeld[p.ID] = p.HeldAt != nil
	}
	// A node is effectively held when it, anything containing it, or its
	// project carries a hold. The walk goes to the root, as the query's
	// does.
	held := map[string]bool{}
	for _, n := range st.nodes {
		held[n.ID] = projectHeld[n.ProjectID]
		for at, ok := n.ID, true; ok && !held[n.ID]; {
			node, exists := byID[at]
			if !exists {
				break
			}
			if node.HeldAt != nil {
				held[n.ID] = true
				break
			}
			if node.ParentID == nil {
				break
			}
			at, ok = *node.ParentID, true
		}
	}
	hasOpenChild := map[string]bool{}
	for _, n := range st.nodes {
		if n.ParentID != nil && n.Status == "open" {
			hasOpenChild[*n.ParentID] = true
		}
	}
	blocked := map[string]bool{}
	for _, e := range st.edges {
		if e.Kind != "blocks" {
			continue
		}
		b := byID[e.FromNode]
		gate := b.AssigneeKind != nil && *b.AssigneeKind == domain.AssigneeUser
		if b.Status == "open" || (gate && b.Status != "done") {
			blocked[e.ToNode] = true
		}
	}

	var ready []store.Node
	for _, n := range st.nodes {
		if n.Status == "open" && !archived[n.ProjectID] && !blocked[n.ID] && !leased[n.ID] && !held[n.ID] && !hasOpenChild[n.ID] {
			ready = append(ready, n)
		}
	}
	return ready
}

// ids renders nodes by id, sorted, for set comparison.
func ids(nodes []store.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	slices.Sort(out)
	return out
}

// queueOrder sorts ready nodes the way the ordered query promises: most
// urgent, then oldest, then by id.
func queueOrder(nodes []store.Node) []store.Node {
	out := slices.Clone(nodes)
	slices.SortFunc(out, func(a, b store.Node) int {
		if c := strings.Compare(a.Priority, b.Priority); c != 0 {
			return c
		}
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func describeNodes(nodes []store.Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, fmt.Sprintf("#%d", n.Number))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// checkAcyclic asserts the wait relation has no cycle, by the independent
// walk and by the domain's own check over every node.
func (h *history) checkAcyclic(st *state) {
	if cycle := st.findCycle(); cycle != nil {
		h.fatalf("the wait relation has a cycle: %s", strings.Join(cycle, " -> "))
	}
	all := make([]string, 0, len(st.nodes))
	for _, n := range st.nodes {
		all = append(all, n.ID)
	}
	err := h.s.WithTx(h.ctx, func(ctx context.Context, tx store.Tx) error {
		return domain.CheckAcyclic(ctx, tx, h.org, all)
	})
	if err != nil {
		h.fatalf("CheckAcyclic disagrees with the independent walk: %v", err)
	}
}

// checkReady asserts the domain's ready view equals the recomputation,
// as a set over the organization and as a queue over the project.
func (h *history) checkReady(st *state) {
	want := st.recomputeReady(time.Now())
	if got, exp := ids(st.readySet), ids(want); !slices.Equal(got, exp) {
		h.fatalf("ready set drifted\n  query:       %s\n  recomputed:  %s", describeNodes(st.readySet), describeNodes(want))
	}
	ordered := queueOrder(want)
	if got, exp := ids(st.readyOrdered), ids(ordered); !slices.Equal(got, exp) {
		h.fatalf("ordered ready view drifted\n  query:       %s\n  recomputed:  %s", describeNodes(st.readyOrdered), describeNodes(ordered))
	}
	for i := range ordered {
		if st.readyOrdered[i].ID != ordered[i].ID {
			h.fatalf("ready order drifted\n  query:       %s\n  recomputed:  %s", describeNodes(st.readyOrdered), describeNodes(ordered))
		}
	}
}

// checkDerived asserts the three answers to "is this node ready" agree:
// membership in the ready set, the Ready flag on the node's derived state,
// and Explain reporting no reason it is not.
//
// They are three separate statements of one rule. The ready set is a SQL
// conjunction of anti-joins, the flag is a Go expression over
// independently selected booleans, and Explain runs the same fragments
// un-negated. A readiness condition added to one and not the others
// compiles and passes every example spec, so this is what catches it.
//
// Everything is read in one transaction, because the conditions include a
// lease expiring and the three answers must be taken from one snapshot.
func (h *history) checkDerived(st *state) {
	type answer struct {
		inSet   bool
		flag    bool
		explain bool
		reasons []domain.Reason
	}
	answers := map[string]answer{}
	err := h.s.WithTx(h.ctx, func(ctx context.Context, tx store.Tx) error {
		ready, err := domain.ReadySet(ctx, tx, h.org, domain.ReadyQuery{})
		if err != nil {
			return err
		}
		inSet := map[string]bool{}
		for _, n := range ready {
			inSet[n.ID] = true
		}
		derived, err := domain.Derive(ctx, tx, h.org, st.nodes)
		if err != nil {
			return err
		}
		for _, n := range st.nodes {
			e, err := domain.Explain(ctx, tx, h.org, n.ID)
			if err != nil {
				return err
			}
			answers[n.ID] = answer{
				inSet:   inSet[n.ID],
				flag:    derived[n.ID].Ready,
				explain: e.Ready(),
				reasons: e.Reasons,
			}
		}
		return nil
	})
	if err != nil {
		h.fatalf("derive and explain: %v", err)
	}
	for _, n := range st.nodes {
		a := answers[n.ID]
		if a.inSet == a.flag && a.flag == a.explain {
			continue
		}
		h.fatalf("the three readiness answers disagree for #%d\n  ready set:   %t\n  Ready flag:  %t\n  Explain:     %t (%s)",
			n.Number, a.inSet, a.flag, a.explain, describeReasons(a.reasons))
	}
}

// describeReasons renders why a node is not ready, for a failure message.
func describeReasons(reasons []domain.Reason) string {
	if len(reasons) == 0 {
		return "no reasons"
	}
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, string(r.Kind))
	}
	return strings.Join(parts, ", ")
}

// newHistory starts a run in a fresh organization with one project, a
// user to lay out the work, and an agent to claim it.
func newHistory(ctx context.Context, t *rapid.T, s *store.Store) *history {
	h := &history{ctx: ctx, t: t, s: s}
	err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		org, err := tx.CreateOrganization(ctx, store.CreateOrganizationParams{ID: newULID(), Name: "acme"})
		if err != nil {
			return err
		}
		h.org = org.ID
		p, err := tx.CreateProject(ctx, store.CreateProjectParams{ID: newULID(), OrgID: h.org, Slug: "platform", Title: "platform", Priority: "p2"})
		if err != nil {
			return err
		}
		h.project = p.ID
		for _, kind := range []string{"user", "agent"} {
			pr, err := tx.CreatePrincipal(ctx, store.CreatePrincipalParams{ID: newULID(), OrgID: h.org, Kind: kind})
			if err != nil {
				return err
			}
			actor := domain.Actor{PrincipalID: pr.ID, PrincipalKind: pr.Kind, IdentityScheme: "unix"}
			if kind == "user" {
				h.user = actor
			} else {
				h.agent = actor
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("set up organization: %v", err)
	}
	return h
}

// run performs one random history, checking each invariant after every
// step.
func run(ctx context.Context, t *rapid.T, s *store.Store, check ...func(*history, *state)) {
	h := newHistory(ctx, t, s)
	for range rapid.IntRange(1, 12).Draw(t, "steps") {
		h.step()
		st := h.load()
		for _, c := range check {
			c(h, st)
		}
	}
}

// rapidTB adapts Ginkgo's testing surface to rapid's. Ginkgo fails a spec
// the moment Errorf is called, which would cut rapid's report off before
// it replays the shrunk counterexample; here the report is collected and
// the spec fails once, with all of it, when rapid says it is done.
type rapidTB struct {
	GinkgoTInterface
	failed []string
}

func (tb *rapidTB) Errorf(format string, args ...any) {
	tb.failed = append(tb.failed, fmt.Sprintf(format, args...))
}

func (tb *rapidTB) Error(args ...any) { tb.failed = append(tb.failed, fmt.Sprint(args...)) }

func (tb *rapidTB) Fatalf(format string, args ...any) {
	tb.Errorf(format, args...)
	tb.FailNow()
}

func (tb *rapidTB) Fatal(args ...any) {
	tb.Error(args...)
	tb.FailNow()
}

func (tb *rapidTB) Fail() {}

func (tb *rapidTB) Failed() bool { return len(tb.failed) > 0 }

func (tb *rapidTB) FailNow() {
	tb.GinkgoTInterface.Fatalf("%s", strings.Join(tb.failed, "\n"))
}

// checkProperty runs prop under rapid, wired to the current spec.
func checkProperty(prop func(*rapid.T)) {
	GinkgoHelper()
	rapid.Check(&rapidTB{GinkgoTInterface: GinkgoT()}, prop)
}

var _ = Describe("Graph properties", Label(testsupport.DBLabel), func() {
	var s *store.Store

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))

		// The scenarios ask for 1000 histories; rapid defaults to 100. A
		// value given on the command line wins.
		f := flag.Lookup("rapid.checks")
		Expect(f).NotTo(BeNil())
		if f.Value.String() == f.DefValue {
			Expect(flag.Set(f.Name, propertyChecks)).To(Succeed())
			DeferCleanup(func() { Expect(flag.Set(f.Name, f.DefValue)).To(Succeed()) })
		}
	})

	It("Randomized graphs stay acyclic", func(ctx SpecContext) {
		By("given 1000 random patch sequences")
		By("then no accepted sequence produces a cycle")
		checkProperty(func(t *rapid.T) {
			run(ctx, t, s, (*history).checkAcyclic)
		})
	})

	It("Derived state never drifts", func(ctx SpecContext) {
		By("given 1000 random patch sequences")
		By("then the ready query result matches an independent recomputation every time")
		checkProperty(func(t *rapid.T) {
			run(ctx, t, s, (*history).checkReady)
		})
	})

	It("The ready set, the Ready flag, and Explain agree", func(ctx SpecContext) {
		By("given 1000 random patch sequences")
		By("then every node is ready in all three views or in none of them")
		checkProperty(func(t *rapid.T) {
			run(ctx, t, s, (*history).checkDerived)
		})
	})
})
