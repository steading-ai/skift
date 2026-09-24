package domain_test

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// addComment is an add_comment operation starting a thread on node.
func addComment(alias, node, body string) domain.Op {
	return domain.Op{AddComment: &domain.AddComment{Alias: alias, Node: node, Body: body}}
}

// reply is an add_comment operation answering the comment parent names.
func reply(alias, node, body, parent string) domain.Op {
	return domain.Op{AddComment: &domain.AddComment{Alias: alias, Node: node, Body: body, InReplyTo: parent}}
}

func editComment(comment, body string) domain.Op {
	return domain.Op{EditComment: &domain.EditComment{Comment: comment, Body: body}}
}

func resolveComment(comment string) domain.Op {
	return domain.Op{ResolveComment: &domain.ResolveComment{Comment: comment}}
}

// readComment reads one comment with its revisions in a transaction of
// its own.
func (g *graph) readComment(ctx context.Context, id string) *domain.CommentHistory {
	GinkgoHelper()
	var h *domain.CommentHistory
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		h, err = domain.ReadComment(ctx, tx, g.org, id)
		return err
	})).To(Succeed())
	return h
}

// comments reads a node's comments, a full page, in a transaction of its
// own.
func (g *graph) comments(ctx context.Context, ref string) []domain.CommentHistory {
	GinkgoHelper()
	var cs []domain.CommentHistory
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		cs, err = domain.Comments(ctx, tx, g.org, g.id(ref), 0, 0)
		return err
	})).To(Succeed())
	return cs
}

// commentsSummary reads the comments hint for a node in a transaction of
// its own.
func (g *graph) commentsSummary(ctx context.Context, ref string) domain.CommentsHint {
	GinkgoHelper()
	var h domain.CommentsHint
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		h, err = domain.CommentsSummary(ctx, tx, g.org, g.id(ref))
		return err
	})).To(Succeed())
	return h
}

func (g *graph) countComments(ctx context.Context) int {
	GinkgoHelper()
	var n int
	Expect(g.s.Pool().QueryRow(ctx, `SELECT count(*) FROM comments WHERE org_id = $1`, g.org).Scan(&n)).To(Succeed())
	return n
}

// bodies renders comments as their current bodies, in order.
func bodies(cs []domain.CommentHistory) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

var _ = Describe("Comments with revision history", Label(testsupport.DBLabel), func() {
	var (
		s     *store.Store
		g     *graph
		actor domain.Actor
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		actor = g.principal(ctx)
		g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))
	})

	Describe("Scenarios", func() {
		It("An edited comment keeps its history", func(ctx SpecContext) {
			By("given a comment edited twice")
			first := g.mustApply(ctx, actor, "add", addComment("c", "#1", "first draft"))
			id := first.CommentAliases["c"]
			Expect(id).NotTo(BeEmpty())
			g.mustApply(ctx, actor, "edit-1", editComment(id, "second draft"))
			g.mustApply(ctx, actor, "edit-2", editComment(id, "third draft"))

			By("when I read it")
			h := g.readComment(ctx, id)

			By("then the current body and two prior revisions are returned in order")
			Expect(h.Body).To(Equal("third draft"))
			Expect(h.Author).To(Equal(actor.PrincipalID))
			Expect(h.Revisions).To(HaveLen(2))
			Expect(h.Revisions[0].Revision).To(Equal(int32(1)))
			Expect(h.Revisions[0].Body).To(Equal("first draft"))
			Expect(h.Revisions[1].Revision).To(Equal(int32(2)))
			Expect(h.Revisions[1].Body).To(Equal("second draft"))
			Expect(h.Revisions[1].CreatedAt).To(BeTemporally(">=", h.Revisions[0].CreatedAt))
		})

		It("A review thread reads in order", func(ctx SpecContext) {
			By("given a comment and two replies")
			r := g.mustApply(ctx, actor, "thread",
				addComment("q", "#1", "Should this be encrypted at rest?"),
				reply("a1", "#1", "Yes, the token store holds refresh tokens.", "q"),
				reply("a2", "#1", "Agreed; adding it to the acceptance criteria.", "q"),
			)
			Expect(r.Comments).To(HaveLen(3))
			root, one, two := r.CommentAliases["q"], r.CommentAliases["a1"], r.CommentAliases["a2"]

			By("when I read the node")
			cs := g.comments(ctx, "#1")

			By("then the thread is reconstructable from in_reply_to")
			Expect(bodies(cs)).To(Equal([]string{
				"Should this be encrypted at rest?",
				"Yes, the token store holds refresh tokens.",
				"Agreed; adding it to the acceptance criteria.",
			}))
			Expect(cs[0].InReplyTo).To(BeNil())
			Expect(cs[1].InReplyTo).To(HaveValue(Equal(root)))
			Expect(cs[2].InReplyTo).To(HaveValue(Equal(root)))

			threads := domain.Threads(cs)
			Expect(threads).To(HaveLen(1))
			Expect(threads[0].ID).To(Equal(root))
			Expect(threads[0].Replies).To(HaveLen(2))
			Expect(threads[0].Replies[0].ID).To(Equal(one))
			Expect(threads[0].Replies[1].ID).To(Equal(two))
			Expect(threads[0].Replies[0].Replies).To(BeEmpty())
		})
	})

	Describe("Editing", func() {
		It("preserves the prior body with its author and timestamp", func(ctx SpecContext) {
			bob := g.principal(ctx)
			added := g.mustApply(ctx, bob, "add", addComment("c", "#1", "as bob wrote it"))
			id := added.CommentAliases["c"]
			written := added.Comments[0].CreatedAt

			g.mustApply(ctx, bob, "edit", editComment(id, "as bob rewrote it"))

			h := g.readComment(ctx, id)
			Expect(h.Body).To(Equal("as bob rewrote it"))
			Expect(h.Revisions).To(HaveLen(1))
			Expect(h.Revisions[0].Body).To(Equal("as bob wrote it"))
			Expect(h.Revisions[0].Author).To(Equal(bob.PrincipalID))
			Expect(h.Revisions[0].CreatedAt).To(BeTemporally(">=", written))
			Expect(h.Revisions[0].CreatedAt).To(BeTemporally("~", time.Now(), time.Minute))
		})

		It("rejects an edit that changes nothing", func(ctx SpecContext) {
			id := g.mustApply(ctx, actor, "add", addComment("c", "#1", "same")).CommentAliases["c"]
			_, err := g.apply(ctx, actor, "noop", editComment(id, "same"))
			Expect(err).To(MatchError(domain.ErrCommentUnchanged))
			Expect(g.readComment(ctx, id).Revisions).To(BeEmpty())
			Expect(g.countEvents(ctx, "noop")).To(BeZero())
		})

		It("is the author's alone", func(ctx SpecContext) {
			bob := g.principal(ctx)
			id := g.mustApply(ctx, bob, "add", addComment("c", "#1", "bob's words")).CommentAliases["c"]

			_, err := g.apply(ctx, actor, "edit", editComment(id, "not bob's words"))
			Expect(err).To(MatchError(policy.ErrDenied))
			Expect(err.Error()).To(ContainSubstring("only the author"))
			Expect(g.readComment(ctx, id).Body).To(Equal("bob's words"))

			By("while anyone permitted on the node may resolve it")
			r := g.mustApply(ctx, actor, "resolve", resolveComment(id))
			Expect(r.Comments[0].ResolvedBy).To(HaveValue(Equal(actor.PrincipalID)))
		})
	})

	Describe("Resolving", func() {
		It("records who resolved and when", func(ctx SpecContext) {
			id := g.mustApply(ctx, actor, "add", addComment("c", "#1", "open question")).CommentAliases["c"]
			Expect(g.readComment(ctx, id).ResolvedAt).To(BeNil())

			resolver := g.principal(ctx)
			r := g.mustApply(ctx, resolver, "resolve", resolveComment(id))

			h := g.readComment(ctx, id)
			Expect(h.ResolvedBy).To(HaveValue(Equal(resolver.PrincipalID)))
			Expect(h.ResolvedAt).NotTo(BeNil())
			Expect(*h.ResolvedAt).To(BeTemporally("~", time.Now(), time.Minute))
			Expect(r.Comments).To(HaveLen(1))
			Expect(r.Comments[0].ResolvedAt).To(HaveValue(Equal(*h.ResolvedAt)))

			By("and a comment is resolved once")
			_, err := g.apply(ctx, actor, "again", resolveComment(id))
			Expect(err).To(MatchError(domain.ErrCommentResolved))
			Expect(g.readComment(ctx, id).ResolvedBy).To(HaveValue(Equal(resolver.PrincipalID)))
		})
	})

	Describe("Summarizing", func() {
		It("gives nothing for a node with no comments", func(ctx SpecContext) {
			Expect(g.commentsSummary(ctx, "#1")).To(Equal(domain.CommentsHint{}))
		})

		It("counts the unresolved comments and dates the newest of them", func(ctx SpecContext) {
			first := g.mustApply(ctx, actor, "first", addComment("c", "#1", "first")).CommentAliases["c"]
			g.mustApply(ctx, actor, "second", addComment("c", "#1", "second"))
			answer := g.mustApply(ctx, actor, "reply", reply("r", "#1", "a reply", first)).CommentAliases["r"]
			g.mustApply(ctx, actor, "elsewhere", addComment("c", "#2", "on another node"))
			newest := g.mustApply(ctx, actor, "newest", addComment("c", "#1", "newest")).CommentAliases["c"]
			g.mustApply(ctx, actor, "resolve", resolveComment(newest))

			h := g.commentsSummary(ctx, "#1")
			Expect(h.Unresolved).To(Equal(3))
			Expect(h.LatestAt).To(HaveValue(Equal(g.readComment(ctx, answer).CreatedAt)))

			By("and a comment on another node is that node's alone")
			Expect(g.commentsSummary(ctx, "#2").Unresolved).To(Equal(1))
		})

		It("counts a reply whose parent is resolved", func(ctx SpecContext) {
			parent := g.mustApply(ctx, actor, "add", addComment("c", "#1", "question")).CommentAliases["c"]
			answer := g.mustApply(ctx, actor, "reply", reply("r", "#1", "answer", parent)).CommentAliases["r"]
			g.mustApply(ctx, actor, "resolve", resolveComment(parent))

			h := g.commentsSummary(ctx, "#1")
			Expect(h.Unresolved).To(Equal(1))
			Expect(h.LatestAt).To(HaveValue(Equal(g.readComment(ctx, answer).CreatedAt)))
		})

		It("is not moved by an edit", func(ctx SpecContext) {
			id := g.mustApply(ctx, actor, "add", addComment("c", "#1", "draft")).CommentAliases["c"]
			before := g.commentsSummary(ctx, "#1")
			g.mustApply(ctx, actor, "edit", editComment(id, "revised"))

			Expect(g.commentsSummary(ctx, "#1")).To(Equal(before))
		})

		It("gives nothing once every comment is resolved", func(ctx SpecContext) {
			id := g.mustApply(ctx, actor, "add", addComment("c", "#1", "note")).CommentAliases["c"]
			g.mustApply(ctx, actor, "resolve", resolveComment(id))

			Expect(g.commentsSummary(ctx, "#1")).To(Equal(domain.CommentsHint{}))
		})
	})

	Describe("Search", func() {
		It("indexes comment bodies", func(ctx SpecContext) {
			r := g.mustApply(ctx, actor, "add",
				addComment("hit", "#1", "the token store should be encrypted at rest"),
				addComment("miss", "#2", "provision redis for sessions"),
			)
			hit := r.CommentAliases["hit"]

			// Tiny tables would seq-scan regardless; forcing the planner
			// off that path proves the index expression matches the query.
			tx, err := s.Pool().Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer tx.Rollback(ctx) //nolint:errcheck // rollback of a read-only transaction
			_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
			Expect(err).NotTo(HaveOccurred())

			const match = `search_document(body) @@ websearch_to_tsquery('english', $1)`
			rows, err := tx.Query(ctx, `SELECT id FROM comments WHERE org_id = $2 AND `+match, "token store", g.org)
			Expect(err).NotTo(HaveOccurred())
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(ConsistOf(hit))

			planRows, err := tx.Query(ctx, `EXPLAIN SELECT id FROM comments WHERE `+match, "token store")
			Expect(err).NotTo(HaveOccurred())
			lines, err := pgx.CollectRows(planRows, pgx.RowTo[string])
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Join(lines, "\n")).To(ContainSubstring("comments_search_idx"))
		})
	})

	Describe("Events", func() {
		It("are emitted by every comment operation", func(ctx SpecContext) {
			r := g.mustApply(ctx, actor, "all-three",
				addComment("c", "#1", "before"),
				editComment("c", "after"),
				resolveComment("c"),
			)
			id := r.CommentAliases["c"]
			Expect(ops(r.Events)).To(Equal([]string{domain.OpAddComment, domain.OpEditComment, domain.OpResolveComment}))
			Expect(ops(g.eventsFor(ctx, "all-three"))).To(Equal(ops(r.Events)))

			By("each on the comment's node, naming the actor")
			for _, e := range r.Events {
				Expect(e.NodeID).To(HaveValue(Equal(g.id("#1"))))
				Expect(e.PrincipalID).To(Equal(actor.PrincipalID))
				Expect(e.PolicyVersion).To(Equal(pol.Version()))
			}

			By("carrying the comment before and after")
			Expect(r.Events[0].Before).To(BeNil())
			Expect(field(r.Events[0].After, "ID")).To(Equal(id))
			Expect(field(r.Events[0].After, "Body")).To(Equal("before"))
			Expect(field(r.Events[1].Before, "Body")).To(Equal("before"))
			Expect(field(r.Events[1].After, "Body")).To(Equal("after"))
			Expect(field(r.Events[2].Before, "ResolvedBy")).To(BeNil())
			Expect(field(r.Events[2].After, "ResolvedBy")).To(Equal(actor.PrincipalID))

			By("and the node's log shows them beside its own")
			Expect(ops(g.nodeEvents(ctx, "#1"))).To(ContainElements(domain.OpCreateNode, domain.OpAddComment, domain.OpEditComment, domain.OpResolveComment))
			Expect(r.Nodes).To(BeEmpty(), "a comment changes nothing about its node")
			Expect(r.Comments).To(HaveLen(1))
			Expect(r.Comments[0].Body).To(Equal("after"))
		})

		It("replays with the comments the first application returned", func(ctx SpecContext) {
			first := g.mustApply(ctx, actor, "once", addComment("c", "#1", "hello"))
			again := g.mustApply(ctx, actor, "once", addComment("c", "#1", "hello"))
			Expect(again.Replayed).To(BeTrue())
			Expect(again.Comments).To(Equal(first.Comments))
			Expect(again.CommentAliases).To(Equal(first.CommentAliases))
			Expect(g.countComments(ctx)).To(Equal(1))
		})
	})

	Describe("Limits", func() {
		It("bounds comment size by the ceiling", func(ctx SpecContext) {
			limits := domain.ActiveLimits()
			over := strings.Repeat("x", limits.CommentBytes+1)

			By("an add over the ceiling is rejected before anything is written")
			_, err := g.apply(ctx, actor, "big", addComment("c", "#1", over))
			limitErr := asLimitError(err, domain.LimitCommentBytes)
			Expect(limitErr.Max).To(Equal(int64(limits.CommentBytes)))
			Expect(limitErr.Value).To(Equal(int64(limits.CommentBytes + 1)))
			Expect(g.countComments(ctx)).To(BeZero())
			Expect(g.countEvents(ctx, "big")).To(BeZero())
			_, err = s.Queries().GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{OrgID: g.org, RequestID: "big"})
			Expect(err).To(MatchError(pgx.ErrNoRows))

			By("so is an edit over it")
			id := g.mustApply(ctx, actor, "add", addComment("c", "#1", "small")).CommentAliases["c"]
			_, err = g.apply(ctx, actor, "big-edit", editComment(id, over))
			asLimitError(err, domain.LimitCommentBytes)
			Expect(g.readComment(ctx, id).Body).To(Equal("small"))

			By("and one at the ceiling is fine")
			g.mustApply(ctx, actor, "edge", addComment("d", "#1", strings.Repeat("y", limits.CommentBytes)))
			Expect(g.countComments(ctx)).To(Equal(2))
		})

		It("requires a body", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "empty", addComment("c", "#1", ""))
			Expect(err).To(MatchError(domain.ErrCommentBodyRequired))
			id := g.mustApply(ctx, actor, "add", addComment("c", "#1", "words")).CommentAliases["c"]
			_, err = g.apply(ctx, actor, "erase", editComment(id, ""))
			Expect(err).To(MatchError(domain.ErrCommentBodyRequired))
		})
	})

	Describe("Threading", func() {
		It("keeps a reply on its parent's node", func(ctx SpecContext) {
			id := g.mustApply(ctx, actor, "add", addComment("c", "#1", "on node 1")).CommentAliases["c"]
			_, err := g.apply(ctx, actor, "stray", reply("r", "#2", "on node 2", id))
			Expect(err).To(MatchError(domain.ErrReplyAcrossNodes))
			Expect(g.countComments(ctx)).To(Equal(1))
		})

		It("rejects a reference to no comment", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "missing", editComment(newULID(), "x"))
			Expect(err).To(MatchError(domain.ErrCommentNotFound))
			_, err = g.apply(ctx, actor, "unbound", reply("r", "#1", "x", "nowhere"))
			Expect(err).To(MatchError(domain.ErrCommentNotFound))
		})

		It("nests replies to replies", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "deep",
				addComment("q", "#1", "q"),
				reply("a", "#1", "a", "q"),
				reply("b", "#1", "b", "a"),
				addComment("other", "#1", "other"),
			)
			threads := domain.Threads(g.comments(ctx, "#1"))
			Expect(threads).To(HaveLen(2))
			Expect(threads[0].Body).To(Equal("q"))
			Expect(threads[0].Replies).To(HaveLen(1))
			Expect(threads[0].Replies[0].Body).To(Equal("a"))
			Expect(threads[0].Replies[0].Replies).To(HaveLen(1))
			Expect(threads[0].Replies[0].Replies[0].Body).To(Equal("b"))
			Expect(threads[1].Body).To(Equal("other"))
		})
	})

	Describe("Authorization", func() {
		It("lets a lease holder comment on its node, and nobody's else", func(ctx SpecContext) {
			alice := g.agent(ctx)
			held := g.mustClaim(ctx, alice, claimReq("claim", time.Minute))
			Expect(held.Node.Number).To(Equal(int64(1)))

			_, err := g.applyWithLease(ctx, alice, "own", held.Lease.ID, addComment("c", "#1", "progress note"))
			Expect(err).NotTo(HaveOccurred())

			_, err = g.applyWithLease(ctx, alice, "elsewhere", held.Lease.ID, addComment("c", "#2", "drive-by"))
			Expect(err).To(MatchError(policy.ErrDenied))

			_, err = g.apply(ctx, alice, "no-lease", addComment("c", "#1", "without a lease"))
			Expect(err).To(MatchError(policy.ErrDenied))
			Expect(g.countComments(ctx)).To(Equal(1))
		})

		It("does not fence a comment on a held node", func(ctx SpecContext) {
			alice := g.agent(ctx)
			g.mustClaim(ctx, alice, claimReq("claim", time.Minute))

			By("a user reviewing the held node comments without its lease")
			r := g.mustApply(ctx, actor, "review", addComment("c", "#1", "looks off, see line 12"))
			Expect(r.Comments).To(HaveLen(1))

			By("while a write to the node itself still needs the lease")
			_, err := g.apply(ctx, actor, "retitle", update("#1", "Renamed"))
			Expect(err).To(MatchError(domain.ErrLeaseRequired))
		})
	})
})
