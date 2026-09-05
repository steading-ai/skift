package migrations_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/store/migrations"
	"github.com/ivy/dagger/internal/testsupport"
)

var _ = Describe("Nodes, edges, leases", Label(testsupport.DBLabel), func() {
	Describe("Scenarios", func() {
		It("Tenancy mismatch is impossible", func(ctx SpecContext) {
			db := pg.Database(ctx)

			By("given a project belonging to organization A")
			orgA := newOrg(ctx, db)
			project := newProject(ctx, db, orgA, "platform")

			By("when a node is inserted claiming organization B and that project")
			orgB := newOrg(ctx, db)
			err := insertNode(ctx, db, node{orgID: orgB, projectID: project, number: 1})

			By("then the database rejects the insert")
			Expect(err).To(HavePgCode(foreignKeyViolation))
		})

		It("Lease history is retained", func(ctx SpecContext) {
			db := pg.Database(ctx)

			By("given a node that has been claimed and released three times")
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "platform")
			agent := newPrincipal(ctx, db, org, "agent")
			n := newNode(ctx, db, org, project)
			for _, reason := range []string{"crash", "gate", "complete"} {
				release(ctx, db, claim(ctx, db, org, n, agent), reason)
			}

			By("when I read its leases")
			total, live := countLeases(ctx, db, n)

			By("then three rows exist")
			Expect(total).To(Equal(3))
			By("and at most one is live")
			Expect(live).To(BeNumerically("<=", 1))
		})
	})

	Describe("Tenancy", func() {
		It("a node's organization is its project's, even when both exist", func(ctx SpecContext) {
			db := pg.Database(ctx)
			orgA := newOrg(ctx, db)
			orgB := newOrg(ctx, db)
			project := newProject(ctx, db, orgA, "platform")
			Expect(insertNode(ctx, db, node{orgID: orgB, projectID: project, number: 1})).To(HavePgCode(foreignKeyViolation))
			Expect(insertNode(ctx, db, node{orgID: orgA, projectID: project, number: 1})).To(Succeed())
		})

		It("a parent is in the same project", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			first := newProject(ctx, db, org, "first")
			second := newProject(ctx, db, org, "second")
			parent := newNode(ctx, db, org, first)
			Expect(insertNode(ctx, db, node{orgID: org, projectID: second, number: 1, parentID: &parent})).To(HavePgCode(foreignKeyViolation))
			Expect(insertNode(ctx, db, node{orgID: org, projectID: first, number: 2, parentID: &parent})).To(Succeed())
		})

		It("an edge joins nodes of one organization", func(ctx SpecContext) {
			db := pg.Database(ctx)
			orgA := newOrg(ctx, db)
			orgB := newOrg(ctx, db)
			a := newNode(ctx, db, orgA, newProject(ctx, db, orgA, "platform"))
			b := newNode(ctx, db, orgB, newProject(ctx, db, orgB, "platform"))
			Expect(insertEdge(ctx, db, orgA, a, b)).To(HavePgCode(foreignKeyViolation))
			Expect(insertEdge(ctx, db, orgB, a, b)).To(HavePgCode(foreignKeyViolation))
		})

		It("an edge may cross projects within an organization", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			a := newNode(ctx, db, org, newProject(ctx, db, org, "first"))
			b := newNode(ctx, db, org, newProject(ctx, db, org, "second"))
			Expect(insertEdge(ctx, db, org, a, b)).To(Succeed())
		})

		It("a lease belongs to its node's organization and its holder's", func(ctx SpecContext) {
			db := pg.Database(ctx)
			orgA := newOrg(ctx, db)
			orgB := newOrg(ctx, db)
			n := newNode(ctx, db, orgA, newProject(ctx, db, orgA, "platform"))
			agentA := newPrincipal(ctx, db, orgA, "agent")
			agentB := newPrincipal(ctx, db, orgB, "agent")
			_, err := insertLease(ctx, db, orgB, n, agentB)
			Expect(err).To(HavePgCode(foreignKeyViolation), "node from another organization")
			_, err = insertLease(ctx, db, orgA, n, agentB)
			Expect(err).To(HavePgCode(foreignKeyViolation), "holder from another organization")
			_, err = insertLease(ctx, db, orgA, n, agentA)
			Expect(err).NotTo(HaveOccurred())
		})

		It("an assignee is a principal of the node's organization", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "platform")
			stranger := newPrincipal(ctx, db, newOrg(ctx, db), "user")
			user := newPrincipal(ctx, db, org, "user")
			Expect(insertNode(ctx, db, node{orgID: org, projectID: project, number: 1, assigneeKind: ptr("user"), assignee: &stranger})).To(HavePgCode(foreignKeyViolation))
			Expect(insertNode(ctx, db, node{orgID: org, projectID: project, number: 1, assigneeKind: ptr("user"), assignee: &user})).To(Succeed())
		})
	})

	Describe("Keys", func() {
		It("numbers are unique within a project and start at 1", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			first := newProject(ctx, db, org, "first")
			second := newProject(ctx, db, org, "second")
			Expect(insertNode(ctx, db, node{orgID: org, projectID: first, number: 1})).To(Succeed())
			Expect(insertNode(ctx, db, node{orgID: org, projectID: first, number: 1})).To(HavePgCode(uniqueViolation))
			Expect(insertNode(ctx, db, node{orgID: org, projectID: second, number: 1})).To(Succeed(), "numbers are per project")
			Expect(insertNode(ctx, db, node{orgID: org, projectID: first, number: 0})).To(HavePgCode(checkViolation))
		})

		It("identifiers are ULIDs and context ids are minted at creation", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "platform")
			Expect(insertNode(ctx, db, node{id: "not-a-ulid", orgID: org, projectID: project, number: 1})).To(HavePgCode(checkViolation))
			n := newNode(ctx, db, org, project)
			var contextID string
			Expect(db.QueryRow(ctx, `SELECT context_id::text FROM nodes WHERE id = $1`, n).Scan(&contextID)).To(Succeed())
			Expect(contextID).To(HaveLen(36))
		})
	})

	Describe("Only what cannot be derived is stored", func() {
		It("has no blocked, ready, or claimed column", func(ctx SpecContext) {
			db := pg.Database(ctx)
			rows, err := db.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'nodes'`)
			Expect(err).NotTo(HaveOccurred())
			columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
			Expect(err).NotTo(HaveOccurred())
			Expect(columns).NotTo(ContainElements("blocked", "ready", "claimed"))
		})

		It("stores only open, done, and dropped", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "platform")
			for i, status := range []string{"open", "done", "dropped"} {
				Expect(insertNode(ctx, db, node{orgID: org, projectID: project, number: int64(i + 1), status: status})).To(Succeed(), status)
			}
			for _, status := range []string{"blocked", "ready", "in_progress"} {
				Expect(insertNode(ctx, db, node{orgID: org, projectID: project, number: 9, status: status})).To(HavePgCode(checkViolation), status)
			}
		})
	})

	Describe("Edges", func() {
		It("cannot loop a node to itself", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			n := newNode(ctx, db, org, newProject(ctx, db, org, "platform"))
			Expect(insertEdge(ctx, db, org, n, n)).To(HavePgCode(checkViolation))
		})

		It("are unique per pair and kind, and removable", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "platform")
			a := newNode(ctx, db, org, project)
			b := newNode(ctx, db, org, project)
			Expect(insertEdge(ctx, db, org, a, b)).To(Succeed())
			Expect(insertEdge(ctx, db, org, a, b)).To(HavePgCode(uniqueViolation))
			_, err := db.Exec(ctx, `INSERT INTO edges (org_id, from_node, to_node, kind) VALUES ($1, $2, $3, 'relates')`, org, a, b)
			Expect(err).NotTo(HaveOccurred(), "another kind is a different edge")
			_, err = db.Exec(ctx, `DELETE FROM edges WHERE from_node = $1 AND to_node = $2 AND kind = 'blocks'`, a, b)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Leases", func() {
		It("allows many leases per node over time and at most one live", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			agent := newPrincipal(ctx, db, org, "agent")
			n := newNode(ctx, db, org, newProject(ctx, db, org, "platform"))

			By("a second claim while one is live is rejected")
			first := claim(ctx, db, org, n, agent)
			_, err := insertLease(ctx, db, org, n, agent)
			Expect(err).To(HavePgCode(uniqueViolation))

			By("once released, the node can be claimed again")
			release(ctx, db, first, "crash")
			second := claim(ctx, db, org, n, agent)

			By("an expired lease is not live, and the next claim releases it as expired first")
			_, err = db.Exec(ctx, `UPDATE leases SET claimed_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1`, second)
			Expect(err).NotTo(HaveOccurred())
			_, live := countLeases(ctx, db, n)
			Expect(live).To(BeZero())
			_, err = insertLease(ctx, db, org, n, agent)
			Expect(err).To(HavePgCode(uniqueViolation), "the expired lease still needs its expiry recorded")
			release(ctx, db, second, "expired")
			claim(ctx, db, org, n, agent)

			total, live := countLeases(ctx, db, n)
			Expect(total).To(Equal(3))
			Expect(live).To(Equal(1))
		})

		It("keeps a release and its reason together", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			agent := newPrincipal(ctx, db, org, "agent")
			n := newNode(ctx, db, org, newProject(ctx, db, org, "platform"))
			lease := claim(ctx, db, org, n, agent)

			_, err := db.Exec(ctx, `UPDATE leases SET released_at = now() WHERE id = $1`, lease)
			Expect(err).To(HavePgCode(checkViolation), "a release without a reason")
			_, err = db.Exec(ctx, `UPDATE leases SET release_reason = 'complete' WHERE id = $1`, lease)
			Expect(err).To(HavePgCode(checkViolation), "a reason without a release")
			_, err = db.Exec(ctx, `UPDATE leases SET released_at = now(), release_reason = 'bored' WHERE id = $1`, lease)
			Expect(err).To(HavePgCode(checkViolation), "a reason outside the vocabulary")
			release(ctx, db, lease, "complete")
		})

		It("expires after it was claimed", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			agent := newPrincipal(ctx, db, org, "agent")
			n := newNode(ctx, db, org, newProject(ctx, db, org, "platform"))
			_, err := db.Exec(ctx, `
				INSERT INTO leases (id, org_id, node_id, holder, harness, agent_ref, expires_at)
				VALUES ($1, $2, $3, $4, 'claude-code', 'workstation', now() - interval '1 second')`, newULID(), org, n, agent)
			Expect(err).To(HavePgCode(checkViolation))
		})
	})

	Describe("Readiness uses the indexes", func() {
		It("EXPLAIN on the readiness query shows no sequential scan with 10000 nodes in a project", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "big")
			agent := newPrincipal(ctx, db, org, "agent")
			populate(ctx, db, org, project, agent, 10000)
			// Also a small project, so a plan cannot assume one project is
			// the whole table.
			populate(ctx, db, org, newProject(ctx, db, org, "small"), agent, 100)

			_, err := db.Exec(ctx, `ANALYZE`)
			Expect(err).NotTo(HaveOccurred())

			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE project_id = $1`, project).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(10000))

			plan := explain(ctx, db, readinessQuery, project)
			Expect(plan.scans()).NotTo(BeEmpty())
			for _, scan := range plan.scans() {
				Expect(scan).NotTo(ContainSubstring("Seq Scan"), "plan:\n%s", plan.text())
			}
		})
	})

	Describe("Every migration has a tested down", func() {
		It("the graph migrations roll back to the tenancy schema and forward again", func(ctx SpecContext) {
			db := pg.Database(ctx)
			m, err := migrations.Open(ctx, db.Config().ConnString())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(m.Close)

			Expect(m.Versions()).To(ContainElements(int64(4), int64(5), int64(6)))
			before := schema(ctx, db)

			Expect(m.DownTo(ctx, 3)).To(Succeed())
			Expect(tables(ctx, db)).NotTo(ContainElements("nodes", "edges", "leases"))
			Expect(tables(ctx, db)).To(ContainElements("organizations", "projects", "principals"))

			Expect(m.Up(ctx)).To(Succeed())
			Expect(schema(ctx, db)).To(Equal(before))
		})
	})
})

// readinessQuery is the Readiness section of the domain model as SQL: open,
// no open blocker, no live lease, no open children, in priority-then-age
// order within one project. It is paged, as Ready is; an unbounded read of
// most of a project is correctly a hash anti-join over full scans, and no
// index changes that.
const readinessQuery = `
	SELECT n.id, n.number, n.priority, n.created_at
	  FROM nodes n
	 WHERE n.project_id = $1
	   AND n.status = 'open'
	   AND NOT EXISTS (
	         SELECT 1 FROM edges e JOIN nodes b ON b.id = e.from_node
	          WHERE e.to_node = n.id AND e.kind = 'blocks' AND b.status = 'open')
	   AND NOT EXISTS (
	         SELECT 1 FROM leases l
	          WHERE l.node_id = n.id AND ` + liveLease + `)
	   AND NOT EXISTS (
	         SELECT 1 FROM nodes c
	          WHERE c.parent_id = n.id AND c.status = 'open')
	 ORDER BY n.priority, n.created_at
	 LIMIT 100`

// populate fills a project with count nodes shaped like a real backlog:
// most open, some done, chains of blocking edges, a scattering of parents,
// and a few live leases.
func populate(ctx context.Context, db *pgxpool.Pool, org, project, agent string, count int) {
	GinkgoHelper()
	ids := make([]string, count)
	rows := make([][]any, count)
	for i := range count {
		ids[i] = newULID()
		status := "open"
		if i%5 == 4 {
			status = "done"
		}
		rows[i] = []any{ids[i], int64(i + 1), org, project, "node " + ids[i], status, fmt.Sprintf("p%d", i%5)}
	}
	_, err := db.CopyFrom(ctx, pgx.Identifier{"nodes"},
		[]string{"id", "number", "org_id", "project_id", "title", "status", "priority"}, pgx.CopyFromRows(rows))
	Expect(err).NotTo(HaveOccurred())

	var edges [][]any
	for i := 1; i < count; i++ {
		if i%4 != 0 {
			edges = append(edges, []any{org, ids[i-1], ids[i]})
		}
	}
	_, err = db.CopyFrom(ctx, pgx.Identifier{"edges"}, []string{"org_id", "from_node", "to_node"}, pgx.CopyFromRows(edges))
	Expect(err).NotTo(HaveOccurred())

	for i := 10; i < count; i += 10 {
		_, err = db.Exec(ctx, `UPDATE nodes SET parent_id = $2 WHERE id = $1`, ids[i], ids[i-1])
		Expect(err).NotTo(HaveOccurred())
	}
	for i := 0; i < count; i += 50 {
		claim(ctx, db, org, ids[i], agent)
	}
}

// plan is the tree EXPLAIN (FORMAT JSON) returns.
type plan struct {
	root map[string]any
	raw  json.RawMessage
}

func explain(ctx context.Context, db *pgxpool.Pool, query string, args ...any) plan {
	GinkgoHelper()
	var raw json.RawMessage
	Expect(db.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+query, args...).Scan(&raw)).To(Succeed())
	var top []struct {
		Plan map[string]any `json:"Plan"`
	}
	Expect(json.Unmarshal(raw, &top)).To(Succeed())
	Expect(top).To(HaveLen(1))
	return plan{root: top[0].Plan, raw: raw}
}

// scans lists every scan node in the plan as "Node Type on relation".
func (p plan) scans() []string {
	var out []string
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		if rel, ok := n["Relation Name"].(string); ok {
			out = append(out, fmt.Sprintf("%s on %s", n["Node Type"], rel))
		}
		children, _ := n["Plans"].([]any)
		for _, c := range children {
			if m, ok := c.(map[string]any); ok {
				walk(m)
			}
		}
	}
	walk(p.root)
	return out
}

func (p plan) text() string { return string(p.raw) }

func tables(ctx context.Context, db *pgxpool.Pool) []string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public'`)
	Expect(err).NotTo(HaveOccurred())
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return names
}
