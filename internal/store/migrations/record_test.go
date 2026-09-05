package migrations_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/store/migrations"
	"github.com/ivy/dagger/internal/testsupport"
)

var _ = Describe("Events, usage, comments, artifacts", Label(testsupport.DBLabel), func() {
	Describe("Scenarios", func() {
		It("The audit record cannot be rewritten", func(ctx SpecContext) {
			db := pg.Database(ctx)

			By("given an existing event row")
			f := newFixture(ctx, db)
			id := f.event(ctx, "create_node")

			By("when any statement updates or deletes it, the database raises an error")
			for _, statement := range []string{
				`UPDATE events SET op = 'tampered' WHERE id = $1`,
				`UPDATE events SET after = '{"status":"done"}' WHERE id = $1`,
				`DELETE FROM events WHERE id = $1`,
				`DELETE FROM events WHERE org_id = (SELECT org_id FROM events WHERE id = $1)`,
			} {
				_, err := db.Exec(ctx, statement, id)
				Expect(err).To(HavePgCode(restrictViolation), statement)
			}

			By("and the row is unchanged")
			var op string
			Expect(db.QueryRow(ctx, `SELECT op FROM events WHERE id = $1`, id).Scan(&op)).To(Succeed())
			Expect(op).To(Equal("create_node"))
		})

		It("An estimate and an actual coexist for one node", func(ctx SpecContext) {
			db := pg.Database(ctx)

			By("given a node estimated before work and reporting usage after")
			f := newFixture(ctx, db)
			base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
			f.estimate(ctx, "planning", 1000, base)
			f.estimate(ctx, "after the spike", 5000, base.Add(time.Hour))
			lease := claim(ctx, db, f.org, f.node, f.agent)
			f.actual(ctx, lease, 7000, base.Add(2*time.Hour))

			By("when I read its usage rows")
			rows, err := db.Query(ctx, `SELECT kind, input_tokens FROM usage WHERE node_id = $1 ORDER BY created_at`, f.node)
			Expect(err).NotTo(HaveOccurred())
			type row struct {
				Kind   string
				Tokens int64
			}
			usage, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
			Expect(err).NotTo(HaveOccurred())

			By("then both rows exist with different kinds")
			Expect(usage).To(ContainElements(row{"estimate", 1000}, row{"estimate", 5000}, row{"actual", 7000}))

			By("and the most recent estimate is identifiable")
			var basis string
			var tokens int64
			Expect(db.QueryRow(ctx, currentEstimateQuery, f.node).Scan(&basis, &tokens)).To(Succeed())
			Expect(basis).To(Equal("after the spike"))
			Expect(tokens).To(Equal(int64(5000)))
		})
	})

	Describe("Events are append-only", func() {
		It("refuses updates and deletes addressed to the partition itself", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			id := f.event(ctx, "create_node")
			partition := partitionFor(ctx, db, time.Now())
			_, err := db.Exec(ctx, fmt.Sprintf(`UPDATE %s SET op = 'tampered' WHERE id = $1`, partition), id)
			Expect(err).To(HavePgCode(restrictViolation))
			_, err = db.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, partition), id)
			Expect(err).To(HavePgCode(restrictViolation))
		})

		It("refuses truncation", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			f.event(ctx, "create_node")
			_, err := db.Exec(ctx, `TRUNCATE events`)
			Expect(err).To(HavePgCode(restrictViolation))
		})

		It("holds usage and comment revisions to the same rule", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			f.estimate(ctx, "planning", 1000, time.Now())
			comment := f.comment(ctx, "first draft")
			f.edit(ctx, comment, "second draft")

			for _, statement := range []string{
				`UPDATE usage SET input_tokens = 0 WHERE node_id = $1`,
				`DELETE FROM usage WHERE node_id = $1`,
				`UPDATE comment_revisions SET body = '' WHERE comment_id IN (SELECT id FROM comments WHERE node_id = $1)`,
				`DELETE FROM comment_revisions WHERE comment_id IN (SELECT id FROM comments WHERE node_id = $1)`,
			} {
				_, err := db.Exec(ctx, statement, f.node)
				Expect(err).To(HavePgCode(restrictViolation), statement)
			}
		})

		It("records the actor, the transport, and the change", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			id := f.event(ctx, "complete_node")
			var principal, kind, scheme, request string
			var before, after []byte
			Expect(db.QueryRow(ctx, `
				SELECT principal_id, principal_kind, identity_scheme, request_id, before, after
				  FROM events WHERE id = $1`, id).Scan(&principal, &kind, &scheme, &request, &before, &after)).To(Succeed())
			Expect(principal).To(Equal(f.agent))
			Expect(kind).To(Equal("agent"))
			Expect(scheme).To(Equal("unix"))
			Expect(request).To(Equal("req-" + id))
			Expect(before).To(MatchJSON(`{"status": "open"}`))
			Expect(after).To(MatchJSON(`{"status": "done"}`))
		})

		It("scopes an event to one organization", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			other := newOrg(ctx, db)
			_, err := db.Exec(ctx, `
				INSERT INTO events (id, org_id, project_id, node_id, principal_id, principal_kind, identity_scheme, policy_version, op)
				VALUES ($1, $2, $3, $4, $5, 'agent', 'unix', 'sha256:spec', 'create_node')`,
				newULID(), other, f.project, f.node, f.agent)
			Expect(err).To(HavePgCode(foreignKeyViolation))
		})

		It("an event about a node names its project", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			_, err := db.Exec(ctx, `
				INSERT INTO events (id, org_id, node_id, principal_id, principal_kind, identity_scheme, policy_version, op)
				VALUES ($1, $2, $3, $4, 'agent', 'unix', 'sha256:spec', 'create_node')`,
				newULID(), f.org, f.node, f.agent)
			Expect(err).To(HavePgCode(checkViolation))
		})
	})

	Describe("A partition exists for the current period and is created ahead of need", func() {
		It("covers now and the next period after the migrations alone", func(ctx SpecContext) {
			db := pg.Database(ctx)
			now := time.Now().UTC()
			Expect(partitions(ctx, db)).To(ContainElements(
				partitionName(now), partitionName(now.AddDate(0, 1, 0))))
		})

		It("accepts an event stamped in the next period", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			nextMonth := time.Now().UTC().AddDate(0, 1, 0)
			_, err := db.Exec(ctx, `
				INSERT INTO events (id, org_id, project_id, node_id, principal_id, principal_kind, identity_scheme, policy_version, op, occurred_at)
				VALUES ($1, $2, $3, $4, $5, 'agent', 'unix', 'sha256:spec', 'create_node', $6)`,
				newULID(), f.org, f.project, f.node, f.agent, nextMonth)
			Expect(err).NotTo(HaveOccurred())
		})

		It("the routine creates the partitions a future date needs and nothing twice", func(ctx SpecContext) {
			db := pg.Database(ctx)
			future := time.Now().UTC().AddDate(1, 0, 0)
			want := []string{partitionName(future), partitionName(future.AddDate(0, 1, 0)), partitionName(future.AddDate(0, 2, 0))}
			Expect(partitions(ctx, db)).NotTo(ContainElements(want[0], want[1], want[2]))

			By("creating the period holding the date and two ahead")
			Expect(ensurePartitions(ctx, db, future, 2)).To(Equal(want))
			Expect(partitions(ctx, db)).To(ContainElements(want[0], want[1], want[2]))

			By("reporting nothing on a repeat")
			Expect(ensurePartitions(ctx, db, future, 2)).To(BeEmpty())

			By("bounding each partition to one calendar month in UTC")
			var lower, upper time.Time
			Expect(db.QueryRow(ctx, `
				SELECT (regexp_match(pg_get_expr(relpartbound, oid), $$FROM \('([^']+)'\) TO \('([^']+)'\)$$))[1]::timestamptz,
				       (regexp_match(pg_get_expr(relpartbound, oid), $$FROM \('([^']+)'\) TO \('([^']+)'\)$$))[2]::timestamptz
				  FROM pg_class WHERE relname = $1`, want[0]).Scan(&lower, &upper)).To(Succeed())
			start := time.Date(future.Year(), future.Month(), 1, 0, 0, 0, 0, time.UTC)
			Expect(lower.UTC()).To(Equal(start))
			Expect(upper.UTC()).To(Equal(start.AddDate(0, 1, 0)))
		})

		It("a period with no partition rejects the write rather than misfiling it", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			_, err := db.Exec(ctx, `
				INSERT INTO events (id, org_id, project_id, node_id, principal_id, principal_kind, identity_scheme, policy_version, op, occurred_at)
				VALUES ($1, $2, $3, $4, $5, 'agent', 'unix', 'sha256:spec', 'create_node', now() - interval '10 years')`,
				newULID(), f.org, f.project, f.node, f.agent)
			Expect(err).To(HavePgCode(checkViolation))
		})
	})

	Describe("Usage", func() {
		It("an actual is attributed to a lease on its own node", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			other := newNode(ctx, db, f.org, f.project)
			lease := claim(ctx, db, f.org, other, f.agent)
			Expect(f.insertUsage(ctx, usageRow{kind: "actual", leaseID: &lease})).To(HavePgCode(foreignKeyViolation))
			Expect(f.insertUsage(ctx, usageRow{kind: "actual"})).To(HavePgCode(checkViolation))
		})

		It("a basis belongs to an estimate", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			lease := claim(ctx, db, f.org, f.node, f.agent)
			Expect(f.insertUsage(ctx, usageRow{kind: "actual", leaseID: &lease, basis: ptr("guess")})).To(HavePgCode(checkViolation))
			Expect(f.insertUsage(ctx, usageRow{kind: "estimate"})).To(Succeed())
			Expect(f.insertUsage(ctx, usageRow{kind: "estimate", basis: ptr("guess")})).To(Succeed())
		})

		It("counts are never negative", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			Expect(f.insertUsage(ctx, usageRow{kind: "estimate", input: -1})).To(HavePgCode(checkViolation))
		})
	})

	Describe("Comments", func() {
		It("keeps every prior body as a revision", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			comment := f.comment(ctx, "one")
			f.edit(ctx, comment, "two")
			f.edit(ctx, comment, "two")
			f.edit(ctx, comment, "three")

			var body string
			Expect(db.QueryRow(ctx, `SELECT body FROM comments WHERE id = $1`, comment).Scan(&body)).To(Succeed())
			Expect(body).To(Equal("three"))

			rows, err := db.Query(ctx, `SELECT revision, body, author FROM comment_revisions WHERE comment_id = $1 ORDER BY revision`, comment)
			Expect(err).NotTo(HaveOccurred())
			type revision struct {
				Revision int32
				Body     string
				Author   string
			}
			revisions, err := pgx.CollectRows(rows, pgx.RowToStructByPos[revision])
			Expect(err).NotTo(HaveOccurred())
			Expect(revisions).To(Equal([]revision{{1, "one", f.agent}, {2, "two", f.agent}}), "an edit that changes nothing leaves no revision")
		})

		It("threads within one node", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			parent := f.comment(ctx, "question")
			_, err := db.Exec(ctx, `
				INSERT INTO comments (id, org_id, node_id, author, body, in_reply_to)
				VALUES ($1, $2, $3, $4, 'answer', $5)`, newULID(), f.org, f.node, f.agent, parent)
			Expect(err).NotTo(HaveOccurred())

			other := newNode(ctx, db, f.org, f.project)
			_, err = db.Exec(ctx, `
				INSERT INTO comments (id, org_id, node_id, author, body, in_reply_to)
				VALUES ($1, $2, $3, $4, 'stray', $5)`, newULID(), f.org, other, f.agent, parent)
			Expect(err).To(HavePgCode(foreignKeyViolation))
		})

		It("resolution records who and when together", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			comment := f.comment(ctx, "question")
			_, err := db.Exec(ctx, `UPDATE comments SET resolved_at = now() WHERE id = $1`, comment)
			Expect(err).To(HavePgCode(checkViolation))
			_, err = db.Exec(ctx, `UPDATE comments SET resolved_at = now(), resolved_by = $2 WHERE id = $1`, comment, f.user)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Full-text index on node title, node body, comment body, and result body", func() {
		It("has a GIN index on each", func(ctx SpecContext) {
			db := pg.Database(ctx)
			rows, err := db.Query(ctx, `SELECT tablename, indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexdef LIKE '%USING gin%'`)
			Expect(err).NotTo(HaveOccurred())
			defer rows.Close()
			gin := map[string]string{}
			for rows.Next() {
				var table, def string
				Expect(rows.Scan(&table, &def)).To(Succeed())
				gin[table] = def
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			Expect(gin).To(HaveKeyWithValue("nodes", ContainSubstring("node_search_document(title, body)")))
			Expect(gin).To(HaveKeyWithValue("comments", ContainSubstring("search_document(body)")))
			Expect(gin).To(HaveKeyWithValue("results", ContainSubstring("search_document(body)")))
		})

		It("finds a term in a title, a body, a comment, and a result through the index", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			titled := f.insertTitled(ctx, "Spike the token store", "")
			bodied := f.insertTitled(ctx, "Unrelated", "We need a token store for refresh tokens.")
			f.insertTitled(ctx, "Redis", "Provision redis for sessions.")
			comment := f.comment(ctx, "the token store should be encrypted at rest")
			result := f.result(ctx, "Built the token store behind an interface.")

			// Tiny tables would seq-scan regardless; forcing the planner
			// off that path proves the index expression matches the query.
			tx, err := db.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer tx.Rollback(ctx) //nolint:errcheck // rollback of a read-only transaction
			_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
			Expect(err).NotTo(HaveOccurred())

			searches := []struct {
				table, predicate, index string
				want                    []string
			}{
				{"nodes", "node_search_document(title, body)", "nodes_search_idx", []string{titled, bodied}},
				{"comments", "search_document(body)", "comments_search_idx", []string{comment}},
				{"results", "search_document(body)", "results_search_idx", []string{result}},
			}
			for _, s := range searches {
				match := s.predicate + ` @@ websearch_to_tsquery('english', $1)`
				rows, err := tx.Query(ctx, `SELECT id FROM `+s.table+` WHERE org_id = $2 AND `+match, "token store", f.org)
				Expect(err).NotTo(HaveOccurred())
				ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
				Expect(err).NotTo(HaveOccurred())
				Expect(ids).To(ConsistOf(s.want), s.index)

				// Without the org predicate, so the planner cannot reach
				// for the org index instead and the only remaining path
				// is the GIN index on the expression.
				plan := explainText(ctx, tx, `SELECT id FROM `+s.table+` WHERE `+match, "token store")
				Expect(plan).To(ContainSubstring(s.index), "plan:\n%s", plan)
			}
		})
	})

	Describe("Digests are SHA-256, stored canonically", func() {
		It("accepts the canonical form and rejects every other spelling", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			sum := sha256.Sum256([]byte("hello"))
			canonical := "sha256:" + hex.EncodeToString(sum[:])
			Expect(f.insertBlob(ctx, canonical)).To(Succeed())

			for _, spelling := range []string{
				hex.EncodeToString(sum[:]),                              // bare
				"sha256:" + strings.ToUpper(hex.EncodeToString(sum[:])), // uppercase
				"SHA256:" + hex.EncodeToString(sum[:]),                  // uppercase algorithm
				"sha256:" + hex.EncodeToString(sum[:])[:63],             // short
				"sha1:" + hex.EncodeToString(sum[:20]),                  // other algorithm
				"",
			} {
				Expect(f.insertBlob(ctx, spelling)).To(HavePgCode(checkViolation), "%q", spelling)
			}
		})

		It("holds references and request hashes to the same form", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			artifact := f.artifact(ctx, "pull request")
			sum := sha256.Sum256([]byte("abc123"))
			canonical := "sha256:" + hex.EncodeToString(sum[:])
			upper := "sha256:" + strings.ToUpper(hex.EncodeToString(sum[:]))
			Expect(f.insertPart(ctx, artifact, 0, part{kind: "reference", uri: ptr("git+https://example.org/repo#refs/heads/main"), digest: ptr(upper)})).To(HavePgCode(checkViolation))
			Expect(f.insertPart(ctx, artifact, 0, part{kind: "reference", uri: ptr("git+https://example.org/repo#refs/heads/main"), digest: ptr(canonical)})).To(Succeed())
			Expect(f.insertKey(ctx, "req-digest", upper, []byte("{}"))).To(HavePgCode(checkViolation))
			Expect(f.insertKey(ctx, "req-digest", canonical, []byte("{}"))).To(Succeed())
		})
	})

	Describe("Artifacts", func() {
		It("a part is exactly one of text, blob, or reference", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			artifact := f.artifact(ctx, "notes")
			sum := sha256.Sum256([]byte("bytes"))
			digest := "sha256:" + hex.EncodeToString(sum[:])
			Expect(f.insertBlob(ctx, digest)).To(Succeed())
			uri := ptr("oci://registry.example.org/design:v1")

			By("each kind with its own payload")
			Expect(f.insertPart(ctx, artifact, 0, part{kind: "text", text: ptr("inline")})).To(Succeed())
			Expect(f.insertPart(ctx, artifact, 1, part{kind: "blob", blob: &digest})).To(Succeed())
			Expect(f.insertPart(ctx, artifact, 2, part{kind: "reference", uri: uri, digest: &digest})).To(Succeed())

			By("never a payload of another kind, or none, or a reference without a digest")
			for i, p := range []part{
				{kind: "text", blob: &digest},
				{kind: "text", text: ptr("inline"), blob: &digest},
				{kind: "blob"},
				{kind: "blob", blob: &digest, uri: uri},
				{kind: "reference", uri: uri},
				{kind: "reference", digest: &digest},
				{kind: "file", text: ptr("inline")},
			} {
				Expect(f.insertPart(ctx, artifact, 10+i, p)).To(HavePgCode(checkViolation), "%+v", p)
			}
		})

		It("a blob part names a blob the organization holds", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			artifact := f.artifact(ctx, "notes")
			sum := sha256.Sum256([]byte("elsewhere"))
			digest := "sha256:" + hex.EncodeToString(sum[:])
			Expect(f.insertPart(ctx, artifact, 0, part{kind: "blob", blob: &digest})).To(HavePgCode(foreignKeyViolation))

			By("even when another organization holds those bytes")
			other := newFixture(ctx, db)
			Expect(other.insertBlob(ctx, digest)).To(Succeed())
			Expect(f.insertPart(ctx, artifact, 0, part{kind: "blob", blob: &digest})).To(HavePgCode(foreignKeyViolation))
			Expect(f.insertBlob(ctx, digest)).To(Succeed())
			Expect(f.insertPart(ctx, artifact, 0, part{kind: "blob", blob: &digest})).To(Succeed())
		})

		It("metadata is an object", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			_, err := db.Exec(ctx, `
				INSERT INTO artifacts (id, org_id, node_id, name, metadata, created_by)
				VALUES ($1, $2, $3, 'notes', '[1]', $4)`, newULID(), f.org, f.node, f.agent)
			Expect(err).To(HavePgCode(checkViolation))
		})
	})

	Describe("Replaying a request id with a different request hash is distinguishable from replaying an identical one", func() {
		It("tells a repeat from a conflict", func(ctx SpecContext) {
			db := pg.Database(ctx)
			f := newFixture(ctx, db)
			original := requestHash("create node A")
			Expect(f.insertKey(ctx, "req-1", original, []byte("response-1"))).To(Succeed())

			By("an identical replay finds a matching hash and the stored response")
			same, response := f.replay(ctx, "req-1", original)
			Expect(same).To(BeTrue())
			Expect(response).To(Equal([]byte("response-1")))

			By("a replay with different content finds the key with a mismatching hash")
			same, _ = f.replay(ctx, "req-1", requestHash("create node B"))
			Expect(same).To(BeFalse())

			By("and the same request id is free in another organization")
			other := newFixture(ctx, db)
			Expect(other.insertKey(ctx, "req-1", requestHash("something else"), []byte("response-2"))).To(Succeed())

			By("while a second row for the id in the same organization is refused")
			Expect(f.insertKey(ctx, "req-1", requestHash("create node B"), []byte("response-3"))).To(HavePgCode(uniqueViolation))
		})
	})

	Describe("Every migration has a tested down", func() {
		It("the record migrations roll back to the graph schema and forward again", func(ctx SpecContext) {
			db := pg.Database(ctx)
			m, err := migrations.Open(ctx, db.Config().ConnString())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(m.Close)

			Expect(m.Versions()).To(ContainElements(int64(7), int64(8), int64(9), int64(10), int64(11), int64(12), int64(13)))
			before := schema(ctx, db)

			Expect(m.DownTo(ctx, 6)).To(Succeed())
			Expect(tables(ctx, db)).NotTo(ContainElements(
				"events", "usage", "comments", "comment_revisions", "artifacts", "artifact_parts", "blobs", "results", "idempotency_keys"))
			Expect(tables(ctx, db)).To(ContainElements("nodes", "edges", "leases"))
			Expect(partitions(ctx, db)).To(BeEmpty())

			Expect(m.Up(ctx)).To(Succeed())
			Expect(schema(ctx, db)).To(Equal(before))
		})
	})
})

// restrictViolation is the SQLSTATE the append-only triggers raise.
const restrictViolation = "23001"

// currentEstimateQuery derives a node's current estimate: the most recent
// estimate row, by time then id.
const currentEstimateQuery = `
	SELECT basis, input_tokens FROM usage
	 WHERE node_id = $1 AND kind = 'estimate'
	 ORDER BY created_at DESC, id DESC
	 LIMIT 1`

// fixture is one organization with a project, a node, an agent, and a user:
// the smallest world the record tables can be written into.
type fixture struct {
	db                              *pgxpool.Pool
	org, project, node, agent, user string
}

func newFixture(ctx context.Context, db *pgxpool.Pool) fixture {
	GinkgoHelper()
	f := fixture{db: db}
	f.org = newOrg(ctx, db)
	f.project = newProject(ctx, db, f.org, "platform")
	f.node = newNode(ctx, db, f.org, f.project)
	f.agent = newPrincipal(ctx, db, f.org, "agent")
	f.user = newPrincipal(ctx, db, f.org, "user")
	return f
}

// event appends an event by the agent, over unix, on the fixture's node,
// and returns its id. The request id is derived from the event id so a
// spec can check it round-trips.
func (f fixture) event(ctx context.Context, op string) string {
	GinkgoHelper()
	id := newULID()
	_, err := f.db.Exec(ctx, `
		INSERT INTO events (id, org_id, project_id, node_id, principal_id, principal_kind, identity_scheme, policy_version, request_id, op, before, after)
		VALUES ($1, $2, $3, $4, $5, 'agent', 'unix', 'sha256:spec', $6, $7, '{"status": "open"}', '{"status": "done"}')`,
		id, f.org, f.project, f.node, f.agent, "req-"+id, op)
	Expect(err).NotTo(HaveOccurred())
	return id
}

type usageRow struct {
	kind    string
	leaseID *string
	basis   *string
	input   int64
	at      *time.Time
}

func (f fixture) insertUsage(ctx context.Context, u usageRow) error {
	_, err := f.db.Exec(ctx, `
		INSERT INTO usage (id, org_id, node_id, lease_id, principal, model, input_tokens, kind, basis, created_at)
		VALUES ($1, $2, $3, $4, $5, 'claude', $6, $7, $8, coalesce($9, now()))`,
		newULID(), f.org, f.node, u.leaseID, f.agent, u.input, u.kind, u.basis, u.at)
	return err
}

func (f fixture) estimate(ctx context.Context, basis string, tokens int64, at time.Time) {
	GinkgoHelper()
	Expect(f.insertUsage(ctx, usageRow{kind: "estimate", basis: &basis, input: tokens, at: &at})).To(Succeed())
}

func (f fixture) actual(ctx context.Context, leaseID string, tokens int64, at time.Time) {
	GinkgoHelper()
	Expect(f.insertUsage(ctx, usageRow{kind: "actual", leaseID: &leaseID, input: tokens, at: &at})).To(Succeed())
}

func (f fixture) comment(ctx context.Context, body string) string {
	GinkgoHelper()
	id := newULID()
	_, err := f.db.Exec(ctx, `INSERT INTO comments (id, org_id, node_id, author, body) VALUES ($1, $2, $3, $4, $5)`,
		id, f.org, f.node, f.agent, body)
	Expect(err).NotTo(HaveOccurred())
	return id
}

func (f fixture) edit(ctx context.Context, commentID, body string) {
	GinkgoHelper()
	_, err := f.db.Exec(ctx, `UPDATE comments SET body = $2 WHERE id = $1`, commentID, body)
	Expect(err).NotTo(HaveOccurred())
}

func (f fixture) result(ctx context.Context, body string) string {
	GinkgoHelper()
	id := newULID()
	_, err := f.db.Exec(ctx, `INSERT INTO results (id, org_id, node_id, body, completed_by) VALUES ($1, $2, $3, $4, $5)`,
		id, f.org, f.node, body, f.agent)
	Expect(err).NotTo(HaveOccurred())
	return id
}

// insertTitled adds a node with the given title and body to the fixture's
// project.
func (f fixture) insertTitled(ctx context.Context, title, body string) string {
	GinkgoHelper()
	id := newULID()
	_, err := f.db.Exec(ctx, `
		INSERT INTO nodes (id, number, org_id, project_id, title, body)
		VALUES ($1, (SELECT coalesce(max(number), 0) + 1 FROM nodes WHERE project_id = $3), $2, $3, $4, $5)`,
		id, f.org, f.project, title, body)
	Expect(err).NotTo(HaveOccurred())
	return id
}

func (f fixture) insertBlob(ctx context.Context, digest string) error {
	_, err := f.db.Exec(ctx, `INSERT INTO blobs (org_id, digest, media_type, size, backend) VALUES ($1, $2, 'application/octet-stream', 5, 'filesystem')`,
		f.org, digest)
	return err
}

func (f fixture) artifact(ctx context.Context, name string) string {
	GinkgoHelper()
	id := newULID()
	_, err := f.db.Exec(ctx, `INSERT INTO artifacts (id, org_id, node_id, name, created_by) VALUES ($1, $2, $3, $4, $5)`,
		id, f.org, f.node, name, f.agent)
	Expect(err).NotTo(HaveOccurred())
	return id
}

type part struct {
	kind                    string
	text, blob, uri, digest *string
}

func (f fixture) insertPart(ctx context.Context, artifactID string, position int, p part) error {
	_, err := f.db.Exec(ctx, `
		INSERT INTO artifact_parts (org_id, artifact_id, position, kind, text, blob_digest, reference_uri, reference_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		f.org, artifactID, position, p.kind, p.text, p.blob, p.uri, p.digest)
	return err
}

func (f fixture) insertKey(ctx context.Context, requestID, hash string, response []byte) error {
	_, err := f.db.Exec(ctx, `INSERT INTO idempotency_keys (org_id, request_id, request_hash, response) VALUES ($1, $2, $3, $4)`,
		f.org, requestID, hash, response)
	return err
}

// replay is the lookup a handler makes before applying a request: does the
// key exist, and if so, does the stored hash match the incoming one.
func (f fixture) replay(ctx context.Context, requestID, hash string) (identical bool, response []byte) {
	GinkgoHelper()
	Expect(f.db.QueryRow(ctx, `
		SELECT request_hash = $3, response FROM idempotency_keys
		 WHERE org_id = $1 AND request_id = $2`, f.org, requestID, hash).Scan(&identical, &response)).To(Succeed())
	return identical, response
}

func requestHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// partitionName is the events partition holding t, as the routine names it.
func partitionName(t time.Time) string {
	return "events_" + t.UTC().Format("200601")
}

// partitionFor returns the partition that actually holds t, from the
// catalog rather than the naming convention.
func partitionFor(ctx context.Context, db *pgxpool.Pool, t time.Time) string {
	GinkgoHelper()
	var name string
	Expect(db.QueryRow(ctx, `
		SELECT c.relname
		  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		 WHERE i.inhparent = 'events'::regclass
		   AND c.relname = $1`, partitionName(t)).Scan(&name)).To(Succeed())
	return name
}

// partitions lists the partitions of events.
func partitions(ctx context.Context, db *pgxpool.Pool) []string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `
		SELECT c.relname
		  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = 'events'
		 ORDER BY 1`)
	Expect(err).NotTo(HaveOccurred())
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return names
}

func ensurePartitions(ctx context.Context, db *pgxpool.Pool, at time.Time, ahead int) []string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `SELECT ensure_event_partitions($1, $2)`, at, ahead)
	Expect(err).NotTo(HaveOccurred())
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return names
}

// explainText returns the plain-text plan for query inside tx, so session
// settings such as enable_seqscan apply.
func explainText(ctx context.Context, tx pgx.Tx, query string, args ...any) string {
	GinkgoHelper()
	rows, err := tx.Query(ctx, `EXPLAIN (FORMAT TEXT) `+query, args...)
	Expect(err).NotTo(HaveOccurred())
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return strings.Join(lines, "\n")
}
