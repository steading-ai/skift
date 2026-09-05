package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/store"
)

// Errors a patch is rejected with before any operation runs.
var (
	// ErrNoRequestID: every patch carries an idempotency key.
	ErrNoRequestID = errors.New("a patch needs a request id")
	// ErrNoOperations: a patch is a mutation, so an empty one is a mistake.
	ErrNoOperations = errors.New("a patch needs at least one operation")
	// ErrProjectArchived: an archived project is read-only.
	ErrProjectArchived = errors.New("project is archived")
)

// Patch is one atomic mutation of the graph. See the Patch section of
// docs/domain-model.md. Its JSON form is the canonical encoding the
// idempotency hash is taken over, so two patches with the same fields hash
// the same however they were built.
//
// A patch carries no actor. The principal that applied it is an [Actor]
// the transport resolved, passed to [Apply] beside the patch and recorded
// on every event; a principal field in a request body has nothing here to
// land on. See docs/adrs/006-provenance-not-authority.md.
type Patch struct {
	// RequestID is the client-supplied idempotency key.
	RequestID string `json:"request_id"`
	// Project is the project the patch applies to: a slug, `org/slug`, or
	// an id. Operations do not repeat it, and a bare `#7` resolves here.
	Project string `json:"project"`
	// LeaseID is the fencing token of the lease the caller holds, when it
	// holds one. It must be live and the caller's, and it must be the
	// current lease of every held node the patch writes to; the patch
	// renews it. See the Fencing section of docs/domain-model.md.
	LeaseID string `json:"lease_id,omitempty"`
	// Ops are applied in order.
	Ops []Op `json:"ops"`
}

// Op is one operation. Exactly one field is set.
type Op struct {
	CreateNode   *CreateNode   `json:"create_node,omitempty"`
	UpdateNode   *UpdateNode   `json:"update_node,omitempty"`
	DropNode     *DropNode     `json:"drop_node,omitempty"`
	CompleteNode *CompleteNode `json:"complete_node,omitempty"`
	ReopenNode   *ReopenNode   `json:"reopen_node,omitempty"`
	AddEdge      *AddEdge      `json:"add_edge,omitempty"`
	RemoveEdge   *RemoveEdge   `json:"remove_edge,omitempty"`

	AttachArtifact *AttachArtifact `json:"attach_artifact,omitempty"`

	AddComment     *AddComment     `json:"add_comment,omitempty"`
	EditComment    *EditComment    `json:"edit_comment,omitempty"`
	ResolveComment *ResolveComment `json:"resolve_comment,omitempty"`
}

// Name is the operation's name as events record it, or empty when no
// field is set.
func (o Op) Name() string {
	switch {
	case o.CreateNode != nil:
		return OpCreateNode
	case o.UpdateNode != nil:
		return OpUpdateNode
	case o.DropNode != nil:
		return OpDropNode
	case o.CompleteNode != nil:
		return OpCompleteNode
	case o.ReopenNode != nil:
		return OpReopenNode
	case o.AddEdge != nil:
		return OpAddEdge
	case o.RemoveEdge != nil:
		return OpRemoveEdge
	case o.AttachArtifact != nil:
		return OpAttachArtifact
	case o.AddComment != nil:
		return OpAddComment
	case o.EditComment != nil:
		return OpEditComment
	case o.ResolveComment != nil:
		return OpResolveComment
	}
	return ""
}

// validate rejects an operation that cannot be applied however the graph
// stands: no operation set, a body missing or over its ceiling, or a
// transition missing the reason it must carry.
func (o Op) validate(l Limits) error {
	switch {
	case o.Name() == "":
		return errors.New("no operation set")
	case o.CreateNode != nil:
		return exceeded(LimitBodyBytes, int64(len(o.CreateNode.Body)), int64(l.BodyBytes))
	case o.UpdateNode != nil && o.UpdateNode.Body != nil:
		return exceeded(LimitBodyBytes, int64(len(*o.UpdateNode.Body)), int64(l.BodyBytes))
	case o.DropNode != nil:
		return o.DropNode.validate()
	case o.CompleteNode != nil:
		return o.CompleteNode.validate(l)
	case o.ReopenNode != nil && o.ReopenNode.Reason == "":
		return ErrReasonRequired
	case o.AttachArtifact != nil:
		return o.AttachArtifact.validate(l)
	case o.AddComment != nil:
		return validateCommentBody(o.AddComment.Body, l)
	case o.EditComment != nil:
		return validateCommentBody(o.EditComment.Body, l)
	}
	return nil
}

// Operation names, as written to events.op.
const (
	OpCreateNode   = "create_node"
	OpUpdateNode   = "update_node"
	OpDropNode     = "drop_node"
	OpCompleteNode = "complete_node"
	OpReopenNode   = "reopen_node"
	OpAddEdge      = "add_edge"
	OpRemoveEdge   = "remove_edge"
	// The attach_artifact operation is named in artifact.go, and comment
	// operations in comment.go.
)

// CreateNode creates a node. The server assigns its id and number; later
// operations in the patch refer to it by Alias.
type CreateNode struct {
	// Alias is a client-chosen name usable as a node reference by any
	// later operation in the same patch. Optional, and never stored.
	Alias string `json:"alias,omitempty"`
	// Parent is a node reference. Empty for a root node.
	Parent string `json:"parent,omitempty"`
	// BlockedBy is shorthand for one add_edge from each reference into the
	// new node.
	BlockedBy []string `json:"blocked_by,omitempty"`
	// Kind defaults to task.
	Kind  string `json:"kind,omitempty"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// Priority defaults to p2.
	Priority string `json:"priority,omitempty"`
	// AssigneeKind is empty, [AssigneeAgent], or [AssigneeUser].
	AssigneeKind string   `json:"assignee_kind,omitempty"`
	Assignee     string   `json:"assignee,omitempty"`
	Labels       []string `json:"labels,omitempty"`
}

// UpdateNode changes the mutable fields of a node. A nil field is left
// alone. Status is not here: it changes only through complete, drop, and
// reopen.
type UpdateNode struct {
	// Node is a node reference.
	Node string `json:"node"`
	// Parent set to the empty string makes the node a root.
	Parent   *string `json:"parent,omitempty"`
	Kind     *string `json:"kind,omitempty"`
	Title    *string `json:"title,omitempty"`
	Body     *string `json:"body,omitempty"`
	Priority *string `json:"priority,omitempty"`
	// AssigneeKind set to the empty string makes the node agent-eligible.
	AssigneeKind *string `json:"assignee_kind,omitempty"`
	// Assignee set to the empty string unassigns.
	Assignee *string `json:"assignee,omitempty"`
	// Labels replaces the label set; an empty slice clears it.
	Labels *[]string `json:"labels,omitempty"`
}

// DropNode abandons a node with a reason. Without a reason the patch is
// rejected: a drop is an assertion about the work, and the event is where
// the assertion is kept. See Drop semantics in docs/domain-model.md.
type DropNode struct {
	// Node is a node reference.
	Node string `json:"node"`
	// Reason classifies why: one of the Drop constants.
	Reason string `json:"reason"`
	// Detail is free text expanding on the reason. Required when Reason
	// is [DropOther], since the classification then says nothing.
	Detail string `json:"detail,omitempty"`
	// Cascade asserts the functionality is gone: every node this one
	// transitively blocks is dropped in the same patch, each as its own
	// operation, authorized and recorded like one the client wrote.
	Cascade bool `json:"cascade,omitempty"`
}

// Drop reasons, as a drop_node classifies itself.
const (
	// DropSuperseded means other work replaced the node.
	DropSuperseded = "superseded"
	// DropNotViable means the work cannot be done as specified.
	DropNotViable = "not_viable"
	// DropNoLongerRelevant means the need for the work went away.
	DropNoLongerRelevant = "no_longer_relevant"
	// DropOther means Detail is the whole explanation.
	DropOther = "other"
)

func (d *DropNode) validate() error {
	switch d.Reason {
	case DropSuperseded, DropNotViable, DropNoLongerRelevant:
		return nil
	case DropOther:
		if d.Detail == "" {
			return fmt.Errorf("%w: a drop for another reason says which in detail", ErrReasonRequired)
		}
		return nil
	case "":
		return ErrReasonRequired
	}
	return fmt.Errorf("unknown drop reason %q", d.Reason)
}

// eventReason is the text the event records: the classification, and the
// detail after it when there is one.
func (d *DropNode) eventReason() string {
	if d.Detail == "" {
		return d.Reason
	}
	return d.Reason + ": " + d.Detail
}

// ReopenNode moves a done or dropped node back to open, with a reason.
// It is not retroactive: nothing downstream changes.
type ReopenNode struct {
	// Node is a node reference.
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// AddEdge adds a directed edge: From blocks To.
type AddEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind defaults to blocks.
	Kind string `json:"kind,omitempty"`
}

// RemoveEdge removes an edge. Removal is an event.
type RemoveEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind defaults to blocks.
	Kind string `json:"kind,omitempty"`
}

// Result is what a patch produced: the state of everything it touched,
// after it was applied. It is what the idempotency key stores, so a
// retried patch gets it back verbatim.
type Result struct {
	// Nodes is every node the patch created or changed, in first-touched
	// order, as it stands after the patch.
	Nodes []store.Node `json:"nodes"`
	// Aliases maps each alias a create_node declared to the `slug#number`
	// reference of the node it created.
	Aliases map[string]string `json:"aliases"`
	// Comments is every comment the patch added or changed, in
	// first-touched order, as it stands after the patch.
	Comments []store.Comment `json:"comments"`
	// CommentAliases maps each alias an add_comment declared to the id of
	// the comment it created.
	CommentAliases map[string]string `json:"comment_aliases"`
	// Events are the events the patch wrote, in operation order.
	Events []store.Event `json:"events"`
	// Replayed reports that this is the stored response of an earlier
	// application of the same request, and nothing was written.
	Replayed bool `json:"-"`
}

// RequestConflictError reports a request id seen before with different
// content. A silent replay would return a response to a request that was
// never made.
type RequestConflictError struct {
	RequestID string
}

func (e *RequestConflictError) Error() string {
	return fmt.Sprintf("request %q was already applied with different content", e.RequestID)
}

// OpError reports the operation a patch failed at. It unwraps to the cause.
type OpError struct {
	// Index is the operation's position in the patch, from 0.
	Index int
	Op    string
	Err   error
}

func (e *OpError) Error() string {
	return fmt.Sprintf("operation %d (%s): %v", e.Index, e.Op, e.Err)
}

func (e *OpError) Unwrap() error { return e.Err }

// Apply applies p for actor in one transaction. Every operation commits or
// none does; aliases resolve across the whole patch; the wait relation is
// checked for cycles once, over the resulting graph. Each operation writes
// one event.
//
// Every operation is authorized by pol once resolved, against the state it
// produced, inside the transaction: the operations the client wrote, and
// the ones a cascade expanded them into. A denial is a [*Denial] wrapped in
// the [*OpError] naming the client's operation, and rolls the patch back;
// it is then recorded in policy_denials with the version of the bundle
// that refused it. The events carry the version of the bundle that allowed
// them.
//
// The actor is whatever the transport resolved, never anything the patch
// carries. A patch with no actor, or with a transition missing its reason,
// is rejected before it claims its request id or touches the graph. So is
// one that exceeds a ceiling in [ActiveLimits], with a [*LimitError]
// naming the ceiling: the shape of the whole patch is checked first, then
// each operation's sizes. See limits.go.
//
// A request id seen before with the same content returns the stored
// [Result] with Replayed set and writes nothing. The same id with
// different content is a [*RequestConflictError]. Two applications of one
// request racing each other serialize on the key: the loser waits for the
// winner to commit, then replays its response without repeating the work.
//
// Patches that mutate edges or parents take a transaction-scoped advisory
// lock on the organization, so two of them cannot each see an acyclic
// graph and together commit a cycle. Patches that only create or update
// node fields take none. See the Concurrency section of
// docs/domain-model.md.
//
// Every existing node a patch writes to is fenced: its row is locked, and
// if a live lease holds it the patch must carry that lease's id, else it is
// rejected with [ErrLeaseRequired] or [ErrLeaseMismatch]. A patch carrying
// a lease id that is not live and the actor's is rejected with
// [ErrLeaseNotLive] or [ErrNotLeaseHolder] whatever it touches. A patch
// that passes renews its lease to [DefaultLeaseTTL] from now, never
// shortening it.
func Apply(ctx context.Context, s *store.Store, pol *policy.Bundle, orgID string, actor Actor, p Patch) (*Result, error) {
	if p.RequestID == "" {
		return nil, ErrNoRequestID
	}
	if len(p.Ops) == 0 {
		return nil, ErrNoOperations
	}
	if err := actor.validate(); err != nil {
		return nil, err
	}
	limits := ActiveLimits()
	if err := checkPatchShape(p, limits); err != nil {
		return nil, err
	}
	for i, op := range p.Ops {
		if err := op.validate(limits); err != nil {
			name := op.Name()
			if name == "" {
				name = "empty"
			}
			return nil, &OpError{Index: i, Op: name, Err: err}
		}
	}
	hash, err := requestHash(p)
	if err != nil {
		return nil, err
	}

	var result *Result
	for attempt := 0; ; attempt++ {
		err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
			result, err = applyOnce(ctx, tx, pol, orgID, actor, p, hash)
			return err
		})
		if isKeyConflict(err) && attempt == 0 {
			continue
		}
		if err != nil {
			return nil, auditDenial(ctx, s, orgID, actor, &p.RequestID, err)
		}
		return result, nil
	}
}

// applyOnce is one attempt at the patch inside tx.
func applyOnce(ctx context.Context, tx store.Tx, pol *policy.Bundle, orgID string, actor Actor, p Patch, hash string) (*Result, error) {
	result, replayed, err := withRequest(ctx, tx, orgID, p.RequestID, hash, func(ctx context.Context) (*Result, error) {
		if mutatesWaits(p) {
			if err := lockOrganization(ctx, tx, orgID); err != nil {
				return nil, err
			}
		}

		a, err := newApplier(ctx, tx, pol, orgID, actor, p)
		if err != nil {
			return nil, err
		}
		for i, op := range p.Ops {
			if err := a.apply(ctx, op); err != nil {
				return nil, &OpError{Index: i, Op: op.Name(), Err: err}
			}
		}
		if err := CheckAcyclic(ctx, tx, orgID, a.touched); err != nil {
			return nil, err
		}
		if err := a.renewLease(ctx); err != nil {
			return nil, err
		}
		return a.result(), nil
	})
	if err != nil {
		return nil, err
	}
	result.Replayed = replayed
	return result, nil
}

// withRequest runs one mutation under its idempotency key. A request seen
// before with the same content returns its stored response, decoded as T,
// with replayed set; seen with different content, a
// [*RequestConflictError]. Otherwise the key is claimed before any work,
// run's result is stored as the response, and the caller receives exactly
// the bytes a retry will, so the two responses are identical rather than
// merely equivalent.
//
// A concurrent application of the same request blocks on the key row
// until this transaction ends: on commit it fails with a key conflict, and
// the caller restarts it to replay; on rollback it proceeds as the first
// application.
func withRequest[T any](ctx context.Context, tx store.Tx, orgID, requestID, hash string, run func(context.Context) (*T, error)) (result *T, replayed bool, err error) {
	stored, err := storedResponse(ctx, tx, orgID, requestID, hash)
	if err != nil {
		return nil, false, err
	}
	if stored != nil {
		result, err = decodeResponse[T](stored)
		return result, true, err
	}
	if _, err := tx.CreateIdempotencyKey(ctx, store.CreateIdempotencyKeyParams{
		OrgID: orgID, RequestID: requestID, RequestHash: hash, Response: []byte("null"),
	}); err != nil {
		return nil, false, fmt.Errorf("claim request %q: %w", requestID, err)
	}

	fresh, err := run(ctx)
	if err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(fresh)
	if err != nil {
		return nil, false, fmt.Errorf("encode response: %w", err)
	}
	if _, err := tx.SetIdempotencyKeyResponse(ctx, store.SetIdempotencyKeyResponseParams{
		OrgID: orgID, RequestID: requestID, Response: encoded,
	}); err != nil {
		return nil, false, fmt.Errorf("record request %q: %w", requestID, err)
	}
	result, err = decodeResponse[T](encoded)
	return result, false, err
}

// storedResponse returns the stored response when the request was applied
// before with the same content, nil when it was never seen, and a
// [*RequestConflictError] when it was seen with different content.
func storedResponse(ctx context.Context, tx store.Tx, orgID, requestID, hash string) ([]byte, error) {
	key, err := tx.GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{OrgID: orgID, RequestID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up request %q: %w", requestID, err)
	}
	if key.RequestHash != hash {
		return nil, &RequestConflictError{RequestID: requestID}
	}
	return key.Response, nil
}

func decodeResponse[T any](encoded []byte) (*T, error) {
	var t T
	if err := json.Unmarshal(encoded, &t); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &t, nil
}

// requestHash is the digest the idempotency key stores, in the canonical
// `sha256:` form the schema checks, taken over the request's JSON form.
func requestHash(request any) (string, error) {
	canonical, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// isKeyConflict reports a unique violation on the idempotency key: another
// application of the same request committed first.
func isKeyConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idempotency_keys_pkey"
}

// mutatesWaits reports whether any operation changes the wait relation:
// an edge, or a parent.
func mutatesWaits(p Patch) bool {
	for _, op := range p.Ops {
		switch {
		case op.AddEdge != nil, op.RemoveEdge != nil:
			return true
		case op.CreateNode != nil && (op.CreateNode.Parent != "" || len(op.CreateNode.BlockedBy) > 0):
			return true
		case op.UpdateNode != nil && op.UpdateNode.Parent != nil:
			return true
		}
	}
	return false
}

// lockNamespace is the first half of the advisory lock key, so the lock
// cannot collide with any other use of advisory locks on the server.
const lockNamespace = "dagger:graph"

// lockOrganization takes the organization's graph lock for the rest of the
// transaction. It blocks until any holder commits or rolls back.
func lockOrganization(ctx context.Context, tx store.Tx, orgID string) error {
	if _, err := tx.Tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, lockNamespace, orgID); err != nil {
		return fmt.Errorf("lock organization graph: %w", err)
	}
	return nil
}

// applier carries the state of one patch through its operations.
type applier struct {
	recorder
	project  *store.Project
	resolver *Resolver
	// lease is the live lease the patch carries, or nil.
	lease *store.Lease
	// created are the nodes this patch created: new, so nothing holds them.
	created map[string]bool
	// fenced are the existing nodes already checked and locked.
	fenced map[string]bool
	// nextNumber is the next unused number of the block allocated for the
	// patch's create_node operations.
	nextNumber int64
	// touched are the waiting sides of every wait the patch added, for
	// the cycle check.
	touched []string
	nodes   []store.Node
	index   map[string]int
	aliases map[string]string
	// comments are the comments the patch added or changed, and
	// commentAliases the names add_comment operations bound to theirs.
	comments       []store.Comment
	commentIndex   map[string]int
	commentAliases map[string]string
}

func newApplier(ctx context.Context, tx store.Tx, pol *policy.Bundle, orgID string, actor Actor, p Patch) (*applier, error) {
	project, err := resolveProject(ctx, tx, orgID, p.Project)
	if err != nil {
		return nil, err
	}
	if project.ArchivedAt != nil {
		return nil, fmt.Errorf("project %s: %w", project.Slug, ErrProjectArchived)
	}
	a := &applier{
		recorder: recorder{tx: tx, orgID: orgID, actor: actor, policy: pol, request: &p.RequestID},
		project:  project,
		resolver: NewResolver(tx, orgID, project),
		created:  map[string]bool{}, fenced: map[string]bool{},
		index: map[string]int{}, aliases: map[string]string{},
		commentIndex: map[string]int{}, commentAliases: map[string]string{},
	}
	if p.LeaseID != "" {
		lease, err := heldLease(ctx, tx, orgID, actor, p.LeaseID)
		if err != nil {
			return nil, err
		}
		a.lease = &lease
		a.recorder.lease = &lease
	}
	creates := 0
	for _, op := range p.Ops {
		if op.CreateNode != nil {
			creates++
		}
	}
	if creates > 0 {
		first, err := AllocateNumbers(ctx, tx, orgID, project.ID, creates)
		if err != nil {
			return nil, err
		}
		a.nextNumber = first
	}
	return a, nil
}

// lockNodeSQL locks one node's row for the rest of the transaction. The
// lock is what makes the fence hold until commit: a claim selects with
// SKIP LOCKED and so cannot take the node while a fenced write is in
// flight, and a fenced write cannot proceed on a node a claim is taking.
const lockNodeSQL = `SELECT 1 FROM nodes n WHERE n.org_id = $1 AND n.id = $2 FOR UPDATE`

// heldBySQL is the id of the node's live lease, or null. It runs as a
// statement of its own after the lock, never in the same statement: under
// read committed a statement's snapshot is taken when it starts, and a
// row that is only locked, not updated, is not re-read once the lock is
// granted. Joined to the lock, this would miss a lease a claim committed
// while the lock was waited for.
const heldBySQL = `
SELECT (SELECT l.id FROM leases l
        WHERE l.org_id = $1 AND l.node_id = $2 AND l.released_at IS NULL AND l.expires_at > now())`

// fence admits a write to an existing node: if a live lease holds it, the
// patch must carry that lease. Nodes the patch created are its own, and a
// node is checked once per patch.
func (a *applier) fence(ctx context.Context, nodeID string) error {
	if a.created[nodeID] || a.fenced[nodeID] {
		return nil
	}
	if _, err := a.tx.Tx.Exec(ctx, lockNodeSQL, a.orgID, nodeID); err != nil {
		return fmt.Errorf("fence node %s: %w", nodeID, err)
	}
	var held *string
	if err := a.tx.Tx.QueryRow(ctx, heldBySQL, a.orgID, nodeID).Scan(&held); err != nil {
		return fmt.Errorf("fence node %s: %w", nodeID, err)
	}
	a.fenced[nodeID] = true
	switch {
	case held == nil:
		return nil
	case a.lease == nil:
		return fmt.Errorf("node %s: %w", nodeID, ErrLeaseRequired)
	case *held != a.lease.ID:
		return fmt.Errorf("node %s: %w", nodeID, ErrLeaseMismatch)
	}
	return nil
}

// renewLease is the renewal any write by the holder earns: the lease's
// expiry moves out to [DefaultLeaseTTL] from now, and never back. The
// statement is fenced, so a lease that stopped being live while the patch
// ran rejects the whole patch here.
func (a *applier) renewLease(ctx context.Context) error {
	if a.lease == nil {
		return nil
	}
	_, err := a.tx.ExtendLease(ctx, store.ExtendLeaseParams{OrgID: a.orgID, ID: a.lease.ID, ExpiresAt: time.Now().Add(DefaultLeaseTTL)})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lease %s: %w", a.lease.ID, ErrLeaseNotLive)
	}
	if err != nil {
		return fmt.Errorf("renew lease %s: %w", a.lease.ID, err)
	}
	return nil
}

// ulidShape is the canonical ULID text the schema accepts, used to tell an
// id from a slug when a patch names its project.
var ulidShape = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

// resolveProject finds the patch's project by id, slug, or `org/slug`.
func resolveProject(ctx context.Context, tx store.Tx, orgID, ref string) (*store.Project, error) {
	if ref == "" {
		return nil, ErrNoDefaultProject
	}
	r := NewResolver(tx, orgID, nil)
	parsed := Ref{Project: ref}
	if slash := strings.IndexByte(ref, '/'); slash >= 0 {
		parsed.Org, parsed.Project = ref[:slash], ref[slash+1:]
		ok, err := r.isOwnOrg(ctx, parsed.Org)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &NotFoundError{Ref: parsed, Missing: MissingOrganization}
		}
	} else if ulidShape.MatchString(ref) {
		p, err := tx.GetProject(ctx, store.GetProjectParams{OrgID: orgID, ID: ref})
		if err == nil {
			return &p, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resolve project %s: %w", ref, err)
		}
	}
	return r.projectFor(ctx, parsed)
}

func (a *applier) apply(ctx context.Context, op Op) error {
	switch {
	case op.CreateNode != nil:
		return a.createNode(ctx, op.CreateNode)
	case op.UpdateNode != nil:
		return a.updateNode(ctx, op.UpdateNode)
	case op.DropNode != nil:
		return a.dropNode(ctx, op.DropNode)
	case op.CompleteNode != nil:
		return a.completeNode(ctx, op.CompleteNode)
	case op.ReopenNode != nil:
		return a.reopenNode(ctx, op.ReopenNode)
	case op.AddEdge != nil:
		return a.addEdge(ctx, op.AddEdge)
	case op.RemoveEdge != nil:
		return a.removeEdge(ctx, op.RemoveEdge)
	case op.AttachArtifact != nil:
		return a.attachArtifact(ctx, op.AttachArtifact)
	case op.AddComment != nil:
		return a.addComment(ctx, op.AddComment)
	case op.EditComment != nil:
		return a.editComment(ctx, op.EditComment)
	case op.ResolveComment != nil:
		return a.resolveComment(ctx, op.ResolveComment)
	}
	return errors.New("no operation set")
}

func (a *applier) createNode(ctx context.Context, op *CreateNode) error {
	params := store.CreateNodeParams{
		ID: newULID(), Number: a.nextNumber, OrgID: a.orgID, ProjectID: a.project.ID,
		Kind: op.Kind, Title: op.Title, Body: op.Body, Priority: op.Priority, Labels: op.Labels,
	}
	a.nextNumber++
	if params.Kind == "" {
		params.Kind = "task"
	}
	if params.Priority == "" {
		params.Priority = "p2"
	}
	if params.Labels == nil {
		params.Labels = []string{}
	}
	if op.AssigneeKind != "" {
		params.AssigneeKind = &op.AssigneeKind
	}
	if op.Assignee != "" {
		params.Assignee = &op.Assignee
	}
	if op.Parent != "" {
		parent, err := a.resolver.Resolve(ctx, op.Parent)
		if err != nil {
			return err
		}
		if err := a.fence(ctx, parent.ID); err != nil {
			return err
		}
		params.ParentID = &parent.ID
		a.touched = append(a.touched, parent.ID)
	}
	blockers := make([]Resolved, 0, len(op.BlockedBy))
	for _, ref := range op.BlockedBy {
		b, err := a.resolver.Resolve(ctx, ref)
		if err != nil {
			return err
		}
		blockers = append(blockers, b)
	}

	node, err := a.tx.CreateNode(ctx, params)
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	a.created[node.ID] = true
	if op.Alias != "" {
		if err := a.resolver.Bind(op.Alias, node); err != nil {
			return err
		}
		a.aliases[op.Alias] = NodeRef{Project: a.project.Slug, Number: node.Number}.String()
	}
	a.record(node)
	if err := a.event(ctx, OpCreateNode, node.ID, node.ProjectID, nil, nil, node); err != nil {
		return err
	}
	for _, b := range blockers {
		if err := a.link(ctx, b.ID, node.ID, "blocks"); err != nil {
			return err
		}
	}
	return nil
}

func (a *applier) updateNode(ctx context.Context, op *UpdateNode) error {
	ref, err := a.resolver.Resolve(ctx, op.Node)
	if err != nil {
		return err
	}
	if err := a.fence(ctx, ref.ID); err != nil {
		return err
	}
	before, err := a.tx.GetNode(ctx, store.GetNodeParams{OrgID: a.orgID, ID: ref.ID})
	if err != nil {
		return fmt.Errorf("read node %s: %w", op.Node, err)
	}
	params := store.UpdateNodeParams{
		OrgID: a.orgID, ID: before.ID, ParentID: before.ParentID, Kind: before.Kind, Title: before.Title,
		Body: before.Body, Priority: before.Priority, AssigneeKind: before.AssigneeKind,
		Assignee: before.Assignee, Labels: before.Labels,
	}
	if op.Parent != nil {
		params.ParentID = nil
		if *op.Parent != "" {
			parent, err := a.resolver.Resolve(ctx, *op.Parent)
			if err != nil {
				return err
			}
			if err := a.fence(ctx, parent.ID); err != nil {
				return err
			}
			params.ParentID = &parent.ID
			a.touched = append(a.touched, parent.ID)
		}
	}
	if op.Kind != nil {
		params.Kind = *op.Kind
	}
	if op.Title != nil {
		params.Title = *op.Title
	}
	if op.Body != nil {
		params.Body = *op.Body
	}
	if op.Priority != nil {
		params.Priority = *op.Priority
	}
	if op.AssigneeKind != nil {
		params.AssigneeKind = optional(*op.AssigneeKind)
	}
	if op.Assignee != nil {
		params.Assignee = optional(*op.Assignee)
	}
	if op.Labels != nil {
		params.Labels = *op.Labels
		if params.Labels == nil {
			params.Labels = []string{}
		}
	}
	after, err := a.tx.UpdateNode(ctx, params)
	if err != nil {
		return fmt.Errorf("update node %s: %w", op.Node, err)
	}
	a.record(after)
	return a.event(ctx, OpUpdateNode, after.ID, after.ProjectID, nil, before, after)
}

func (a *applier) dropNode(ctx context.Context, op *DropNode) error {
	reason := op.eventReason()
	notDropped := func(ref string) func(store.Node) error {
		return func(n store.Node) error {
			if n.Status == "dropped" {
				return fmt.Errorf("node %s is already dropped", ref)
			}
			return nil
		}
	}
	resolved, err := a.resolver.Resolve(ctx, op.Node)
	if err != nil {
		return err
	}
	if err := a.transitionNode(ctx, OpDropNode, resolved.ID, op.Node, &reason, notDropped(op.Node), "dropped"); err != nil {
		return err
	}
	if !op.Cascade {
		return nil
	}
	// The subtree this node blocks, walked breadth-first over blocks
	// edges. Each node it reaches is dropped as an operation of its own,
	// so it is fenced, authorized, and recorded like one the client
	// wrote; a node already dropped is passed over but still walked
	// through, since what it blocks is still this node's subtree.
	seen := map[string]bool{resolved.ID: true}
	queue := []string{resolved.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		edges, err := a.tx.ListEdgesOutOf(ctx, store.ListEdgesOutOfParams{OrgID: a.orgID, FromNode: id})
		if err != nil {
			return fmt.Errorf("read edges out of node %s: %w", id, err)
		}
		for _, e := range edges {
			if e.Kind != "blocks" || seen[e.ToNode] {
				continue
			}
			seen[e.ToNode] = true
			queue = append(queue, e.ToNode)
			node, err := a.tx.GetNode(ctx, store.GetNodeParams{OrgID: a.orgID, ID: e.ToNode})
			if err != nil {
				return fmt.Errorf("read node %s: %w", e.ToNode, err)
			}
			if node.Status == "dropped" {
				continue
			}
			display := fmt.Sprintf("%s (blocked by %s)", e.ToNode, op.Node)
			if err := a.transitionNode(ctx, OpDropNode, e.ToNode, display, &reason, notDropped(display), "dropped"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *applier) reopenNode(ctx context.Context, op *ReopenNode) error {
	return a.transition(ctx, OpReopenNode, op.Node, &op.Reason, func(n store.Node) error {
		if n.Status == "open" {
			return fmt.Errorf("node %s is open", op.Node)
		}
		return nil
	}, "open")
}

// transition moves the node ref names to a new status once check accepts
// its current state, and records the move with its reason.
func (a *applier) transition(ctx context.Context, op, ref string, reason *string, check func(store.Node) error, status string) error {
	resolved, err := a.resolver.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	return a.transitionNode(ctx, op, resolved.ID, ref, reason, check, status)
}

// transitionNode is transition on a resolved node. ref is how errors name
// it.
func (a *applier) transitionNode(ctx context.Context, op, id, ref string, reason *string, check func(store.Node) error, status string) error {
	if err := a.fence(ctx, id); err != nil {
		return err
	}
	before, err := a.tx.GetNode(ctx, store.GetNodeParams{OrgID: a.orgID, ID: id})
	if err != nil {
		return fmt.Errorf("read node %s: %w", ref, err)
	}
	if err := check(before); err != nil {
		return err
	}
	after, err := a.tx.SetNodeStatus(ctx, store.SetNodeStatusParams{OrgID: a.orgID, ID: before.ID, Status: status})
	if err != nil {
		return fmt.Errorf("%s %s: %w", op, ref, err)
	}
	a.record(after)
	return a.event(ctx, op, after.ID, after.ProjectID, reason, before, after)
}

func (a *applier) addEdge(ctx context.Context, op *AddEdge) error {
	from, to, kind, err := a.endpoints(ctx, op.From, op.To, op.Kind)
	if err != nil {
		return err
	}
	return a.link(ctx, from.ID, to.ID, kind)
}

// link adds one edge and records it as an event on the waiting side, which
// is the side whose readiness changes and so the side that is fenced.
func (a *applier) link(ctx context.Context, fromID, toID, kind string) error {
	if err := a.fence(ctx, toID); err != nil {
		return err
	}
	edge, err := a.tx.CreateEdge(ctx, store.CreateEdgeParams{OrgID: a.orgID, FromNode: fromID, ToNode: toID, Kind: kind})
	if err != nil {
		return fmt.Errorf("add edge: %w", err)
	}
	a.touched = append(a.touched, toID)
	if err := a.touch(ctx, toID); err != nil {
		return err
	}
	return a.event(ctx, OpAddEdge, toID, a.projectOf(toID), nil, nil, edge)
}

func (a *applier) removeEdge(ctx context.Context, op *RemoveEdge) error {
	from, to, kind, err := a.endpoints(ctx, op.From, op.To, op.Kind)
	if err != nil {
		return err
	}
	if err := a.fence(ctx, to.ID); err != nil {
		return err
	}
	edge := store.Edge{OrgID: a.orgID, FromNode: from.ID, ToNode: to.ID, Kind: kind}
	n, err := a.tx.DeleteEdge(ctx, store.DeleteEdgeParams{OrgID: a.orgID, FromNode: from.ID, ToNode: to.ID, Kind: kind})
	if err != nil {
		return fmt.Errorf("remove edge: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no %s edge from %s to %s", kind, op.From, op.To)
	}
	if err := a.touch(ctx, to.ID); err != nil {
		return err
	}
	return a.event(ctx, OpRemoveEdge, to.ID, a.projectOf(to.ID), nil, edge, nil)
}

func (a *applier) endpoints(ctx context.Context, fromRef, toRef, kind string) (from, to Resolved, k string, err error) {
	if from, err = a.resolver.Resolve(ctx, fromRef); err != nil {
		return from, to, "", err
	}
	if to, err = a.resolver.Resolve(ctx, toRef); err != nil {
		return from, to, "", err
	}
	if kind == "" {
		kind = "blocks"
	}
	return from, to, kind, nil
}

// touch adds a node the patch changed without writing its row, such as
// the target of an edge, to the result.
func (a *applier) touch(ctx context.Context, id string) error {
	if _, ok := a.index[id]; ok {
		return nil
	}
	node, err := a.tx.GetNode(ctx, store.GetNodeParams{OrgID: a.orgID, ID: id})
	if err != nil {
		return fmt.Errorf("read node: %w", err)
	}
	a.record(node)
	return nil
}

// record keeps the latest state of a node the patch touched, in
// first-touched order.
func (a *applier) record(node store.Node) {
	if i, ok := a.index[node.ID]; ok {
		a.nodes[i] = node
		return
	}
	a.index[node.ID] = len(a.nodes)
	a.nodes = append(a.nodes, node)
}

// projectOf is the project of a node the patch already holds.
func (a *applier) projectOf(id string) string {
	return a.nodes[a.index[id]].ProjectID
}

func (a *applier) result() *Result {
	r := &Result{Nodes: a.nodes, Aliases: a.aliases, Comments: a.comments, CommentAliases: a.commentAliases, Events: a.events}
	if r.Nodes == nil {
		r.Nodes = []store.Node{}
	}
	if r.Comments == nil {
		r.Comments = []store.Comment{}
	}
	if r.Events == nil {
		r.Events = []store.Event{}
	}
	return r
}

// optional maps the empty string, which a wire request uses to clear a
// field, to null.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
