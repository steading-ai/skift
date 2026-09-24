package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/steading-ai/skift/internal/store"
)

// A comment is content attached to a node, and its own entity rather than
// a kind of event: people edit, thread, and resolve comments, and events
// are immutable. Every operation on a comment still writes an event, so
// the log records that the comment was added, edited, or resolved, and by
// whom. Editing keeps the prior body as a revision, so nothing is lost.
// See the Comment section of docs/domain-model.md.
//
// A comment is not a write to its node: it changes neither the node's row
// nor its readiness, so a comment on a held node is not fenced by the
// node's lease. Whether a principal may comment at all is the policy's
// question, asked of the node the comment is on.

// Errors a comment operation is rejected with. A body over its ceiling is
// a [*LimitError] naming [LimitCommentBytes]; see limits.go.
var (
	// ErrCommentBodyRequired: a comment is its body, so an empty one is a
	// mistake.
	ErrCommentBodyRequired = errors.New("a comment needs a body")
	// ErrCommentNotFound: the reference names no comment in the
	// organization and no alias in the patch.
	ErrCommentNotFound = errors.New("comment not found")
	// ErrCommentUnchanged: an edit that leaves the body as it was would
	// record an edit with no revision to show for it.
	ErrCommentUnchanged = errors.New("the edit leaves the comment as it was")
	// ErrCommentResolved: a comment is resolved once. Who resolved it and
	// when is the record, and a second resolution would overwrite it.
	ErrCommentResolved = errors.New("comment is already resolved")
	// ErrReplyAcrossNodes: a reply is on the same node as the comment it
	// answers.
	ErrReplyAcrossNodes = errors.New("a reply is on the same node as its parent")
)

// Operation names, as written to events.op.
const (
	OpAddComment     = "add_comment"
	OpEditComment    = "edit_comment"
	OpResolveComment = "resolve_comment"
)

// AddComment attaches a comment to a node, authored by the actor. The
// server assigns its id; later operations in the patch refer to it by
// Alias, so a comment and its replies can arrive in one patch.
type AddComment struct {
	// Alias is a client-chosen name usable as a comment reference by any
	// later operation in the same patch. Optional, and never stored.
	Alias string `json:"alias,omitempty"`
	// Node is a node reference.
	Node string `json:"node"`
	// Body is markdown under [Limits.CommentBytes]. Required.
	Body string `json:"body"`
	// InReplyTo is a comment reference: the id of a comment on the same
	// node, or the alias of one added earlier in the patch. Empty starts
	// a thread.
	InReplyTo string `json:"in_reply_to,omitempty"`
}

// EditComment replaces a comment's body. The prior body is kept as a
// revision with its author and the time it was replaced.
type EditComment struct {
	// Comment is a comment reference.
	Comment string `json:"comment"`
	// Body is the new body, under [Limits.CommentBytes]. Required, and
	// different from the current one.
	Body string `json:"body"`
}

// ResolveComment marks a comment resolved by the actor, now. A comment is
// resolved once.
type ResolveComment struct {
	// Comment is a comment reference.
	Comment string `json:"comment"`
}

// validateCommentBody rejects a body that is empty or over the ceiling.
func validateCommentBody(body string, l Limits) error {
	if body == "" {
		return ErrCommentBodyRequired
	}
	return exceeded(LimitCommentBytes, int64(len(body)), int64(l.CommentBytes))
}

// CommentHistory is a comment as it is read: its current state and every
// prior body, oldest first. Each revision carries the author of the body
// it holds and the time that body was replaced.
type CommentHistory struct {
	store.Comment
	Revisions []store.CommentRevision `json:"revisions"`
}

// Thread is a comment with the replies to it, each with their own, in
// creation order. It is the shape [Threads] rebuilds from in_reply_to.
type Thread struct {
	CommentHistory
	Replies []Thread `json:"replies"`
}

// ReadComment returns one comment with its revisions.
func ReadComment(ctx context.Context, tx store.Tx, orgID, commentID string) (*CommentHistory, error) {
	c, err := tx.GetComment(ctx, store.GetCommentParams{OrgID: orgID, ID: commentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("comment %s: %w", commentID, ErrCommentNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read comment %s: %w", commentID, err)
	}
	return withRevisions(ctx, tx, orgID, c)
}

// Comments returns one page of a node's comments in creation order, each
// with its revisions. limit is bounded by [Limits.PageSize]; zero asks
// for a full page. Creation order is thread order: a reply is always
// created after the comment it answers, so [Threads] over a page never
// meets a reply before its parent unless the parent fell on an earlier
// page.
func Comments(ctx context.Context, tx store.Tx, orgID, nodeID string, limit, offset int) ([]CommentHistory, error) {
	if offset < 0 {
		return nil, errors.New("offset is negative")
	}
	pageSize, err := boundPageSize(limit, ActiveLimits())
	if err != nil {
		return nil, err
	}
	if _, err := tx.GetNode(ctx, store.GetNodeParams{OrgID: orgID, ID: nodeID}); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeNotFound
	} else if err != nil {
		return nil, fmt.Errorf("read node %s: %w", nodeID, err)
	}
	rows, err := tx.ListCommentsForNode(ctx, store.ListCommentsForNodeParams{
		OrgID: orgID, NodeID: nodeID, PageSize: int32(pageSize), PageOffset: int32(offset), //nolint:gosec // both bounded by the page ceiling and its caller
	})
	if err != nil {
		return nil, fmt.Errorf("read comments of node %s: %w", nodeID, err)
	}
	out := make([]CommentHistory, 0, len(rows))
	for _, c := range rows {
		h, err := withRevisions(ctx, tx, orgID, c)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, nil
}

// CommentsHint is what a node's unresolved comments come to without
// carrying them: enough for a claimant to know someone left a note and
// whether it is new, so it can decide whether to read them.
type CommentsHint struct {
	// Unresolved is how many of the node's comments are unresolved,
	// replies counted on their own.
	Unresolved int
	// LatestAt is when the newest unresolved comment was written, else
	// nil.
	LatestAt *time.Time
}

// CommentsSummary is the hint a response carries beside a node. It reads
// one row, never the bodies.
func CommentsSummary(ctx context.Context, tx store.Tx, orgID, nodeID string) (CommentsHint, error) {
	row, err := tx.LatestUnresolvedComment(ctx, store.LatestUnresolvedCommentParams{OrgID: orgID, NodeID: nodeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return CommentsHint{}, nil
	}
	if err != nil {
		return CommentsHint{}, fmt.Errorf("summarize comments of node %s: %w", nodeID, err)
	}
	return CommentsHint{Unresolved: int(row.Unresolved), LatestAt: &row.CreatedAt}, nil
}

func withRevisions(ctx context.Context, tx store.Tx, orgID string, c store.Comment) (*CommentHistory, error) {
	revisions, err := tx.ListCommentRevisions(ctx, store.ListCommentRevisionsParams{OrgID: orgID, CommentID: c.ID})
	if err != nil {
		return nil, fmt.Errorf("read revisions of comment %s: %w", c.ID, err)
	}
	return &CommentHistory{Comment: c, Revisions: revisions}, nil
}

// Threads rebuilds the review threads of a node from in_reply_to. Each
// comment lands under the one it answers, in the order given; a comment
// answering none, or one whose parent is not among the given comments,
// is a root.
func Threads(comments []CommentHistory) []Thread {
	present := make(map[string]bool, len(comments))
	for i := range comments {
		present[comments[i].ID] = true
	}
	children := map[string][]CommentHistory{}
	var roots []CommentHistory
	for _, c := range comments {
		if c.InReplyTo == nil || !present[*c.InReplyTo] {
			roots = append(roots, c)
			continue
		}
		children[*c.InReplyTo] = append(children[*c.InReplyTo], c)
	}
	var build func([]CommentHistory) []Thread
	build = func(cs []CommentHistory) []Thread {
		out := make([]Thread, 0, len(cs))
		for _, c := range cs {
			out = append(out, Thread{CommentHistory: c, Replies: build(children[c.ID])})
		}
		return out
	}
	return build(roots)
}

func (a *applier) addComment(ctx context.Context, op *AddComment) error {
	node, err := a.resolver.Resolve(ctx, op.Node)
	if err != nil {
		return err
	}
	params := store.CreateCommentParams{
		ID: newULID(), OrgID: a.orgID, NodeID: node.ID, Author: a.actor.PrincipalID, Body: op.Body,
	}
	if op.InReplyTo != "" {
		parent, err := a.comment(ctx, op.InReplyTo)
		if err != nil {
			return err
		}
		if parent.NodeID != node.ID {
			return fmt.Errorf("reply to %s on node %s: %w", op.InReplyTo, op.Node, ErrReplyAcrossNodes)
		}
		params.InReplyTo = &parent.ID
	}
	c, err := a.tx.CreateComment(ctx, params)
	if err != nil {
		return fmt.Errorf("add comment: %w", err)
	}
	if op.Alias != "" {
		if err := a.bindComment(op.Alias, c.ID); err != nil {
			return err
		}
	}
	a.recordComment(c)
	return a.event(ctx, OpAddComment, c.NodeID, node.ProjectID, nil, nil, c)
}

func (a *applier) editComment(ctx context.Context, op *EditComment) error {
	before, err := a.comment(ctx, op.Comment)
	if err != nil {
		return err
	}
	if before.Body == op.Body {
		return fmt.Errorf("comment %s: %w", op.Comment, ErrCommentUnchanged)
	}
	after, err := a.tx.UpdateCommentBody(ctx, store.UpdateCommentBodyParams{OrgID: a.orgID, ID: before.ID, Body: op.Body})
	if err != nil {
		return fmt.Errorf("edit comment %s: %w", op.Comment, err)
	}
	a.recordComment(after)
	return a.commentEvent(ctx, OpEditComment, before, after)
}

func (a *applier) resolveComment(ctx context.Context, op *ResolveComment) error {
	before, err := a.comment(ctx, op.Comment)
	if err != nil {
		return err
	}
	if before.ResolvedAt != nil {
		return fmt.Errorf("comment %s: %w", op.Comment, ErrCommentResolved)
	}
	after, err := a.tx.ResolveComment(ctx, store.ResolveCommentParams{OrgID: a.orgID, ID: before.ID, ResolvedBy: &a.actor.PrincipalID})
	if err != nil {
		return fmt.Errorf("resolve comment %s: %w", op.Comment, err)
	}
	a.recordComment(after)
	return a.commentEvent(ctx, OpResolveComment, before, after)
}

// commentEvent records a change to an existing comment on the comment's
// node.
func (a *applier) commentEvent(ctx context.Context, op string, before, after store.Comment) error {
	node, err := a.tx.GetNode(ctx, store.GetNodeParams{OrgID: a.orgID, ID: after.NodeID})
	if err != nil {
		return fmt.Errorf("read node %s: %w", after.NodeID, err)
	}
	return a.event(ctx, op, after.NodeID, node.ProjectID, nil, before, after)
}

// comment resolves a comment reference: an alias bound earlier in the
// patch, else the id of a comment in the organization.
func (a *applier) comment(ctx context.Context, ref string) (store.Comment, error) {
	id := ref
	if bound, ok := a.commentAliases[ref]; ok {
		id = bound
	}
	c, err := a.tx.GetComment(ctx, store.GetCommentParams{OrgID: a.orgID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, fmt.Errorf("comment %s: %w", ref, ErrCommentNotFound)
	}
	if err != nil {
		return c, fmt.Errorf("read comment %s: %w", ref, err)
	}
	return c, nil
}

// bindComment names a comment the patch created, for later operations in
// the same patch.
func (a *applier) bindComment(alias, id string) error {
	if _, dup := a.commentAliases[alias]; dup {
		return fmt.Errorf("comment alias %q is bound twice in one patch", alias)
	}
	a.commentAliases[alias] = id
	return nil
}

// recordComment keeps the latest state of a comment the patch touched, in
// first-touched order.
func (a *applier) recordComment(c store.Comment) {
	if i, ok := a.commentIndex[c.ID]; ok {
		a.comments[i] = c
		return
	}
	a.commentIndex[c.ID] = len(a.comments)
	a.comments = append(a.comments, c)
}
