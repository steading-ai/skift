package domain_test

import (
	"context"
	"fmt"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// createWithBody is a create_node operation with a title and a body.
func createWithBody(alias, title, body string) domain.Op {
	return domain.Op{CreateNode: &domain.CreateNode{Alias: alias, Title: title, Body: body}}
}

// applyIn is apply against another project of the organization, by slug.
func (g *graph) applyIn(ctx context.Context, actor domain.Actor, project, requestID string, ops ...domain.Op) *domain.Result {
	GinkgoHelper()
	r, err := g.applyPatch(ctx, actor, domain.Patch{RequestID: requestID, Project: project, Ops: ops})
	Expect(err).NotTo(HaveOccurred())
	return r
}

// search runs one search under the default policy in a transaction of
// its own.
func (g *graph) search(ctx context.Context, actor domain.Actor, q domain.SearchQuery) ([]domain.Hit, error) {
	return g.searchUnder(ctx, pol, actor, q)
}

func (g *graph) searchUnder(ctx context.Context, bundle *policy.Bundle, actor domain.Actor, q domain.SearchQuery) ([]domain.Hit, error) {
	var hits []domain.Hit
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		hits, err = domain.Search(ctx, tx, bundle, g.org, actor, q)
		return err
	})
	return hits, err
}

func (g *graph) mustSearch(ctx context.Context, actor domain.Actor, q domain.SearchQuery) []domain.Hit {
	GinkgoHelper()
	hits, err := g.search(ctx, actor, q)
	Expect(err).NotTo(HaveOccurred())
	return hits
}

// archivedSince marks a project archived as of `ago` before now.
func (g *graph) archivedSince(ctx context.Context, project string, ago time.Duration) {
	GinkgoHelper()
	_, err := g.s.Pool().Exec(ctx,
		`UPDATE projects SET archived_at = now() - $3::interval WHERE org_id = $1 AND id = $2`,
		g.org, project, ago)
	Expect(err).NotTo(HaveOccurred())
}

// hitIDs renders hits as their node ids, in order.
func hitIDs(hits []domain.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}

// indexScans is how many times the planner has chosen nodes_search_idx
// in this database.
func (g *graph) indexScans(ctx context.Context) int64 {
	GinkgoHelper()
	var n int64
	Expect(g.s.Pool().QueryRow(ctx,
		`SELECT coalesce(idx_scan, 0) FROM pg_stat_user_indexes WHERE indexrelname = 'nodes_search_idx'`).Scan(&n)).To(Succeed())
	return n
}

var _ = Describe("Lexical search", Label(testsupport.DBLabel), func() {
	var (
		s     *store.Store
		g     *graph
		actor domain.Actor
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		actor = g.principal(ctx)
	})

	Describe("Scenarios", func() {
		It("Finding an abandoned spike", func(ctx SpecContext) {
			By("given a project archived six months ago containing a node about token stores")
			spike := g.newProject(ctx, "oauth-spike")
			r := g.applyIn(ctx, actor, "oauth-spike", "spike", createWithBody("ts", "Evaluate a token store",
				"Where should refresh tokens live? The token store must be encrypted at rest."))
			node := r.Nodes[0].ID
			g.archivedSince(ctx, spike, 6*30*24*time.Hour)

			By(`when I search for "token store" with archived projects included`)
			hits := g.mustSearch(ctx, actor, domain.SearchQuery{Query: "token store", IncludeArchived: true})

			By("then the node is returned with a snippet")
			Expect(hitIDs(hits)).To(Equal([]string{node}))
			Expect(hits[0].Snippet).To(ContainSubstring("<b>token</b> <b>store</b>"))
			Expect(hits[0].Source).To(Equal("node"))
		})

		It("Search respects tenancy", func(ctx SpecContext) {
			By(`given two organizations each with a node titled "provision redis"`)
			other := newGraph(ctx, s)
			mine := g.mustApply(ctx, actor, "mine", create("r", "provision redis")).Nodes[0].ID
			theirs := other.mustApply(ctx, other.principal(ctx), "theirs", create("r", "provision redis")).Nodes[0].ID
			Expect(theirs).NotTo(Equal(mine))

			By("when a principal of one searches")
			hits := g.mustSearch(ctx, actor, domain.SearchQuery{Query: "provision redis"})

			By("then only its own organization's node is returned")
			Expect(hitIDs(hits)).To(Equal([]string{mine}))
		})
	})

	It("excludes archived projects by default and includes them by flag", func(ctx SpecContext) {
		spike := g.newProject(ctx, "spike")
		live := g.mustApply(ctx, actor, "live", create("a", "Rotate the signing key")).Nodes[0].ID
		old := g.applyIn(ctx, actor, "spike", "old", create("b", "Rotate the signing key nightly")).Nodes[0].ID
		g.archivedSince(ctx, spike, time.Hour)

		Expect(hitIDs(g.mustSearch(ctx, actor, domain.SearchQuery{Query: "signing key"}))).To(Equal([]string{live}))
		Expect(hitIDs(g.mustSearch(ctx, actor, domain.SearchQuery{Query: "signing key", IncludeArchived: true}))).To(ConsistOf(live, old))
	})

	It("ranks stably and carries a snippet from the best-matching source", func(ctx SpecContext) {
		By("given nodes matching in the title, the body, a comment, and a result")
		r := g.mustApply(ctx, actor, "setup",
			createWithBody("title", "Token store rotation", "Nothing else to say."),
			createWithBody("body", "Cache layer", "Rotate the token store nightly."),
			createWithBody("comment", "Observability", "Dashboards and alerts."),
			createWithBody("result", "Secrets", "Move secrets somewhere safer."),
			createWithBody("none", "Unrelated", "A node about something else entirely."),
		)
		byTitle, byBody, byComment, byResult := r.Nodes[0].ID, r.Nodes[1].ID, r.Nodes[2].ID, r.Nodes[3].ID
		g.mustApply(ctx, actor, "comment", addComment("c", "#3", "The token store holds the refresh secret too."))
		g.mustApply(ctx, actor, "result", complete("#4", "Moved the token store into vault."))

		By("when I search three times")
		first := g.mustSearch(ctx, actor, domain.SearchQuery{Query: "token store"})
		second := g.mustSearch(ctx, actor, domain.SearchQuery{Query: "token store"})
		third := g.mustSearch(ctx, actor, domain.SearchQuery{Query: "token store"})

		By("then the order is the same each time: title, body, then equal ranks by id")
		Expect(hitIDs(first)).To(Equal([]string{byTitle, byBody, byComment, byResult}))
		Expect(hitIDs(second)).To(Equal(hitIDs(first)))
		Expect(hitIDs(third)).To(Equal(hitIDs(first)))
		Expect(first[0].Rank).To(BeNumerically(">", first[1].Rank))
		Expect(first[1].Rank).To(BeNumerically(">", first[2].Rank))
		Expect(first[2].Rank).To(Equal(first[3].Rank))

		By("and every hit carries a snippet from the source that matched")
		Expect(first[0].Source).To(Equal("node"))
		Expect(first[1].Source).To(Equal("node"))
		Expect(first[2].Source).To(Equal("comment"))
		Expect(first[3].Source).To(Equal("result"))
		for _, h := range first {
			Expect(h.Snippet).To(MatchRegexp(`(?i)<b>token</b> <b>store</b>`), h.Title)
		}
		Expect(first[2].Snippet).To(ContainSubstring("refresh secret"))
		Expect(first[3].Snippet).To(ContainSubstring("vault"))
	})

	It("pages the ranked set by limit and offset", func(ctx SpecContext) {
		var ops []domain.Op
		for i := range 5 {
			ops = append(ops, create(fmt.Sprintf("n%d", i), fmt.Sprintf("Paging fixture %d", i)))
		}
		g.mustApply(ctx, actor, "page", ops...)
		all := hitIDs(g.mustSearch(ctx, actor, domain.SearchQuery{Query: "paging fixture"}))
		Expect(all).To(HaveLen(5))
		Expect(hitIDs(g.mustSearch(ctx, actor, domain.SearchQuery{Query: "paging fixture", Limit: 2}))).To(Equal(all[:2]))
		Expect(hitIDs(g.mustSearch(ctx, actor, domain.SearchQuery{Query: "paging fixture", Limit: 2, Offset: 2}))).To(Equal(all[2:4]))
		Expect(hitIDs(g.mustSearch(ctx, actor, domain.SearchQuery{Query: "paging fixture", Limit: 2, Offset: 4}))).To(Equal(all[4:]))

		_, err := g.search(ctx, actor, domain.SearchQuery{Query: "paging fixture", Limit: domain.MaxPageSize + 1})
		var limit *domain.LimitError
		Expect(err).To(BeAssignableToTypeOf(limit))
	})

	It("filters to one project by slug or id", func(ctx SpecContext) {
		g.newProject(ctx, "other")
		here := g.mustApply(ctx, actor, "here", create("a", "Shared phrase")).Nodes[0].ID
		there := g.applyIn(ctx, actor, "other", "there", create("b", "Shared phrase")).Nodes[0].ID

		Expect(hitIDs(g.mustSearch(ctx, actor, domain.SearchQuery{Query: "shared phrase"}))).To(ConsistOf(here, there))
		Expect(hitIDs(g.mustSearch(ctx, actor, domain.SearchQuery{Query: "shared phrase", Project: "other"}))).To(Equal([]string{there}))
		Expect(hitIDs(g.mustSearch(ctx, actor, domain.SearchQuery{Query: "shared phrase", Project: g.project}))).To(Equal([]string{here}))

		_, err := g.search(ctx, actor, domain.SearchQuery{Query: "shared phrase", Project: "nowhere"})
		Expect(err).To(MatchError(domain.ErrProjectNotFound))
	})

	It("rejects a search with nothing to look for", func(ctx SpecContext) {
		_, err := g.search(ctx, actor, domain.SearchQuery{Query: "  "})
		Expect(err).To(MatchError(domain.ErrSearchQueryRequired))
	})

	It("leaves out hits the policy refuses", func(ctx SpecContext) {
		By("given a policy that lets a principal search one project only")
		visible := g.newProject(ctx, "visible")
		shown := g.applyIn(ctx, actor, "visible", "shown", create("a", "Restricted phrase")).Nodes[0].ID
		g.mustApply(ctx, actor, "hidden", create("b", "Restricted phrase"))
		narrow, err := policy.Load(ctx, fstest.MapFS{"narrow.rego": &fstest.MapFile{Data: []byte(fmt.Sprintf(`
package dagger.authz

default allow := false

allow if {
	input.action == "search"
	input.resource.node.project_id == %q
}

decision := {"allow": allow, "reasons": []}
`, visible))}})
		Expect(err).NotTo(HaveOccurred())

		By("when the principal searches under it")
		hits, err := g.searchUnder(ctx, narrow, actor, domain.SearchQuery{Query: "restricted phrase"})
		Expect(err).NotTo(HaveOccurred())

		By("then only the hit the policy allows is returned")
		Expect(hitIDs(hits)).To(Equal([]string{shown}))
	})

	It("answers over 100000 nodes within a bound, through the GIN index", func(ctx SpecContext) {
		By("given 100000 nodes, a handful of them about token stores")
		const total = 100000
		wanted := map[int]bool{17: true, 40000: true, 99998: true}
		var expected []string
		rows := make([][]any, 0, total)
		for i := 1; i <= total; i++ {
			id := domain.NewULID()
			title := fmt.Sprintf("Task %d: refresh the %s cache", i, []string{"session", "profile", "catalog", "pricing"}[i%4])
			body := fmt.Sprintf("Routine work item %d. Rebuild the cache and verify the checksum before closing.", i)
			if wanted[i] {
				body = fmt.Sprintf("Spike %d: the token store must survive a restart without losing refresh tokens.", i)
				expected = append(expected, id)
			}
			rows = append(rows, []any{id, int64(i), g.org, g.project, "task", title, body, "p2", []string{}})
		}
		_, err := s.Pool().CopyFrom(ctx, pgx.Identifier{"nodes"},
			[]string{"id", "number", "org_id", "project_id", "kind", "title", "body", "priority", "labels"},
			pgx.CopyFromRows(rows))
		Expect(err).NotTo(HaveOccurred())
		_, err = s.Pool().Exec(ctx, `ANALYZE nodes`)
		Expect(err).NotTo(HaveOccurred())
		before := g.indexScans(ctx)

		By("when I search for the phrase")
		start := time.Now()
		var hits []domain.Hit
		Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
			hits, err = domain.Search(ctx, tx, pol, g.org, actor, domain.SearchQuery{Query: "token store"})
			if err != nil {
				return err
			}
			// Flush this backend's statistics at commit, so the index
			// scan count below reflects the search just run.
			_, err = tx.Tx.Exec(ctx, `SELECT pg_stat_force_next_flush()`)
			return err
		})).To(Succeed())
		elapsed := time.Since(start)

		By("then the hits come back well within a second and the planner used nodes_search_idx")
		Expect(hitIDs(hits)).To(ConsistOf(expected))
		Expect(elapsed).To(BeNumerically("<", 2*time.Second), "search took %s", elapsed)
		Eventually(func() int64 { return g.indexScans(ctx) }).WithTimeout(10 * time.Second).Should(BeNumerically(">", before))
	})
})
