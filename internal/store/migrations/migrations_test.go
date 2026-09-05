package migrations_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/store/migrations"
	"github.com/ivy/dagger/internal/testsupport"
)

var _ = Describe("Organizations, projects, principals, identities", Label(testsupport.DBLabel), func() {
	Describe("Scenarios", func() {
		It("One principal, several transport identities", func(ctx SpecContext) {
			db := pg.Database(ctx)

			By("given a principal with a unix identity and a tailscale identity")
			org := newOrg(ctx, db)
			principal := newPrincipal(ctx, db, org, "user")
			addIdentity(ctx, db, principal, "unix", "1000@workstation")
			addIdentity(ctx, db, principal, "tailscale", "workstation.tail1234.ts.net")

			By("when either identity is resolved, the same principal id is returned")
			Expect(resolveIdentity(ctx, db, "unix", "1000@workstation")).To(Equal(principal))
			Expect(resolveIdentity(ctx, db, "tailscale", "workstation.tail1234.ts.net")).To(Equal(principal))
		})

		It("Renaming a project keeps old references working", func(ctx SpecContext) {
			db := pg.Database(ctx)

			By("given a project with slug \"oauth-spike\" holding node 7")
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "oauth-spike")
			nodes := standInNodes(ctx, db)
			nodes.insert(ctx, project, 7)

			By("when the project is renamed to \"oauth-migration\"")
			_, err := db.Exec(ctx, `UPDATE projects SET slug = 'oauth-migration' WHERE id = $1`, project)
			Expect(err).NotTo(HaveOccurred())

			By("then \"oauth-spike#7\" still resolves to that node")
			Expect(nodes.resolve(ctx, org, "oauth-spike#7")).To(Equal(nodeKey{project, 7}))
			By("and \"oauth-migration#7\" resolves to the same node")
			Expect(nodes.resolve(ctx, org, "oauth-migration#7")).To(Equal(nodeKey{project, 7}))
		})

		It("Slugs are unique within an organization only", func(ctx SpecContext) {
			db := pg.Database(ctx)

			By("given two organizations")
			first := newOrg(ctx, db)
			second := newOrg(ctx, db)

			By("when each creates a project with slug \"platform\", both succeed")
			Expect(insertProject(ctx, db, newULID(), first, "platform")).To(Succeed())
			Expect(insertProject(ctx, db, newULID(), second, "platform")).To(Succeed())

			By("while a second \"platform\" in one organization does not")
			Expect(insertProject(ctx, db, newULID(), first, "platform")).To(HavePgCode(uniqueViolation))
		})
	})

	Describe("Slugs are bounded at 100 characters and stored lowercase", func() {
		It("accepts a slug of exactly 100 characters", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			Expect(insertProject(ctx, db, newULID(), org, repeat(100))).To(Succeed())
		})

		It("rejects a slug of 101 characters", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			Expect(insertProject(ctx, db, newULID(), org, repeat(101))).To(HavePgCode(checkViolation))
		})

		It("rejects a slug that is not lowercase", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			Expect(insertProject(ctx, db, newULID(), org, "OAuth-Spike")).To(HavePgCode(checkViolation))
		})

		It("rejects an empty slug", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			Expect(insertProject(ctx, db, newULID(), org, "")).To(HavePgCode(checkViolation))
		})

		It("rejects a slug containing a reference delimiter", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			for _, slug := range []string{"acme/platform", "platform#7", "plat form"} {
				Expect(insertProject(ctx, db, newULID(), org, slug)).To(HavePgCode(checkViolation), slug)
			}
		})

		It("holds retired slugs to the same rule", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "platform")
			for _, slug := range []string{repeat(101), "Platform", ""} {
				_, err := db.Exec(ctx, `INSERT INTO project_slug_aliases (org_id, project_id, slug) VALUES ($1, $2, $3)`, org, project, slug)
				Expect(err).To(HavePgCode(checkViolation), "%q", slug)
			}
		})
	})

	Describe("node_counter is a column, never a PostgreSQL sequence", func() {
		It("is an ordinary bigint column starting at 1", func(ctx SpecContext) {
			db := pg.Database(ctx)
			var dataType, columnDefault string
			Expect(db.QueryRow(ctx, `
				SELECT data_type, column_default
				  FROM information_schema.columns
				 WHERE table_name = 'projects' AND column_name = 'node_counter'`).Scan(&dataType, &columnDefault)).To(Succeed())
			Expect(dataType).To(Equal("bigint"))
			Expect(columnDefault).To(Equal("1"))

			var serial *string
			Expect(db.QueryRow(ctx, `SELECT pg_get_serial_sequence('projects', 'node_counter')`).Scan(&serial)).To(Succeed())
			Expect(serial).To(BeNil(), "node_counter must not be backed by a sequence")
		})

		It("creates no sequence at all", func(ctx SpecContext) {
			db := pg.Database(ctx)
			var sequences []string
			rows, err := db.Query(ctx, `SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' AND sequencename NOT LIKE 'goose_%'`)
			Expect(err).NotTo(HaveOccurred())
			for rows.Next() {
				var name string
				Expect(rows.Scan(&name)).To(Succeed())
				sequences = append(sequences, name)
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			Expect(sequences).To(BeEmpty())
		})

		It("creating a project performs no DDL", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			before := countRelations(ctx, db)
			for range 50 {
				newProject(ctx, db, org, "project-"+strings.ToLower(newULID()))
			}
			Expect(countRelations(ctx, db)).To(Equal(before), "creating projects must not create relations")
		})

		It("advances the counter in place", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "platform")
			var first, next int64
			Expect(db.QueryRow(ctx, `
				UPDATE projects SET node_counter = node_counter + 5
				 WHERE id = $1
				RETURNING node_counter - 5, node_counter`, project).Scan(&first, &next)).To(Succeed())
			Expect(first).To(Equal(int64(1)))
			Expect(next).To(Equal(int64(6)))
		})
	})

	Describe("A retired slug resolves to the project that used to hold it", func() {
		It("resolves through the alias after a rename", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "oauth-spike")

			_, err := db.Exec(ctx, `UPDATE projects SET slug = 'oauth-migration' WHERE id = $1`, project)
			Expect(err).NotTo(HaveOccurred())

			id, retired := resolveSlug(ctx, db, org, "oauth-spike")
			Expect(id).To(Equal(project))
			Expect(retired).To(BeTrue())

			id, retired = resolveSlug(ctx, db, org, "oauth-migration")
			Expect(id).To(Equal(project))
			Expect(retired).To(BeFalse())
		})

		It("keeps every slug a project has ever had", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "one")
			for _, slug := range []string{"two", "three", "four"} {
				_, err := db.Exec(ctx, `UPDATE projects SET slug = $2 WHERE id = $1`, project, slug)
				Expect(err).NotTo(HaveOccurred())
			}
			for _, slug := range []string{"one", "two", "three", "four"} {
				id, _ := resolveSlug(ctx, db, org, slug)
				Expect(id).To(Equal(project), slug)
			}
		})

		It("refuses another project the retired slug", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "oauth-spike")
			_, err := db.Exec(ctx, `UPDATE projects SET slug = 'oauth-migration' WHERE id = $1`, project)
			Expect(err).NotTo(HaveOccurred())

			By("neither by creation")
			Expect(insertProject(ctx, db, newULID(), org, "oauth-spike")).To(HavePgCode(uniqueViolation))

			By("nor by rename")
			other := newProject(ctx, db, org, "other")
			_, err = db.Exec(ctx, `UPDATE projects SET slug = 'oauth-spike' WHERE id = $1`, other)
			Expect(err).To(HavePgCode(uniqueViolation))

			By("nor by retiring a slug another project holds")
			_, err = db.Exec(ctx, `INSERT INTO project_slug_aliases (org_id, project_id, slug) VALUES ($1, $2, 'other')`, org, project)
			Expect(err).To(HavePgCode(uniqueViolation))

			By("while a different organization is free to use it")
			Expect(insertProject(ctx, db, newULID(), newOrg(ctx, db), "oauth-spike")).To(Succeed())
		})

		It("lets a project take back a slug it retired", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "oauth-spike")
			_, err := db.Exec(ctx, `UPDATE projects SET slug = 'oauth-migration' WHERE id = $1`, project)
			Expect(err).NotTo(HaveOccurred())
			_, err = db.Exec(ctx, `UPDATE projects SET slug = 'oauth-spike' WHERE id = $1`, project)
			Expect(err).NotTo(HaveOccurred())

			id, retired := resolveSlug(ctx, db, org, "oauth-spike")
			Expect(id).To(Equal(project))
			Expect(retired).To(BeFalse(), "the slug is live again, not an alias")
			id, retired = resolveSlug(ctx, db, org, "oauth-migration")
			Expect(id).To(Equal(project))
			Expect(retired).To(BeTrue())
		})

		It("drops the aliases with the project", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			project := newProject(ctx, db, org, "oauth-spike")
			_, err := db.Exec(ctx, `UPDATE projects SET slug = 'oauth-migration' WHERE id = $1`, project)
			Expect(err).NotTo(HaveOccurred())
			_, err = db.Exec(ctx, `DELETE FROM projects WHERE id = $1`, project)
			Expect(err).NotTo(HaveOccurred())
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM project_slugs WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())
		})
	})

	Describe("Tenancy", func() {
		It("a project belongs to an existing organization", func(ctx SpecContext) {
			db := pg.Database(ctx)
			Expect(insertProject(ctx, db, newULID(), newULID(), "platform")).To(HavePgCode("23503"))
		})

		It("an alias belongs to its project's organization", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			other := newOrg(ctx, db)
			project := newProject(ctx, db, org, "platform")
			_, err := db.Exec(ctx, `INSERT INTO project_slug_aliases (org_id, project_id, slug) VALUES ($1, $2, 'old')`, other, project)
			Expect(err).To(HavePgCode("23503"))
		})

		It("an identity is unique across principals", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			first := newPrincipal(ctx, db, org, "user")
			second := newPrincipal(ctx, db, org, "agent")
			addIdentity(ctx, db, first, "unix", "1000@workstation")
			_, err := db.Exec(ctx, `INSERT INTO principal_identities (principal_id, scheme, value) VALUES ($1, 'unix', '1000@workstation')`, second)
			Expect(err).To(HavePgCode(uniqueViolation))
		})

		It("accepts a scheme nobody has used before", func(ctx SpecContext) {
			db := pg.Database(ctx)
			org := newOrg(ctx, db)
			principal := newPrincipal(ctx, db, org, "service")
			addIdentity(ctx, db, principal, "spiffe", "spiffe://example.org/notifier")
			Expect(resolveIdentity(ctx, db, "spiffe", "spiffe://example.org/notifier")).To(Equal(principal))
		})

		It("identifiers are ULIDs", func(ctx SpecContext) {
			db := pg.Database(ctx)
			_, err := db.Exec(ctx, `INSERT INTO organizations (id, name) VALUES ('not-a-ulid', 'acme')`)
			Expect(err).To(HavePgCode(checkViolation))
		})
	})

	Describe("Every migration has a tested down", func() {
		It("each down restores the schema its up found, all the way to empty", func(ctx SpecContext) {
			db := pg.Database(ctx)
			m, err := migrations.Open(ctx, db.Config().ConnString())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(m.Close)

			versions := m.Versions()
			Expect(versions).NotTo(BeEmpty())
			top, err := m.Version(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(top).To(Equal(versions[len(versions)-1]), "the template carries every migration")

			By("recording the schema at every version on the way down")
			snapshots := map[int64][]string{}
			for i := len(versions) - 1; i >= 0; i-- {
				snapshots[versions[i]] = schema(ctx, db)
				previous := int64(0)
				if i > 0 {
					previous = versions[i-1]
				}
				Expect(m.DownTo(ctx, previous)).To(Succeed(), "down from %d", versions[i])
				Expect(m.Version(ctx)).To(Equal(previous))
			}

			By("nothing but goose's own bookkeeping survives at version 0")
			for _, object := range schema(ctx, db) {
				Expect(object).To(ContainSubstring("goose_db_version"), "left behind: %s", object)
			}

			By("every up reproduces the schema recorded for its version")
			for _, version := range versions {
				Expect(m.UpTo(ctx, version)).To(Succeed(), "up to %d", version)
				Expect(schema(ctx, db)).To(Equal(snapshots[version]), "schema at version %d", version)
			}
		})

		It("applying migrations twice is a no-op", func(ctx SpecContext) {
			db := pg.Database(ctx)
			before := schema(ctx, db)
			Expect(migrations.Migrate(ctx, db.Config().ConnString())).To(Succeed())
			Expect(schema(ctx, db)).To(Equal(before))
		})
	})
})

// schema describes every user-visible object in the public schema, in a
// stable order, so two states can be compared exactly.
func schema(ctx context.Context, db *pgxpool.Pool) []string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `
		SELECT 'relation ' || c.relname || ' ' || c.relkind::text
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public'
		UNION ALL
		SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type
		       || ' default=' || coalesce(column_default, '') || ' nullable=' || is_nullable
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		UNION ALL
		SELECT 'constraint ' || conrelid::regclass::text || '.' || conname || ' ' || pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE connamespace = 'public'::regnamespace
		UNION ALL
		SELECT 'index ' || indexdef FROM pg_indexes WHERE schemaname = 'public'
		UNION ALL
		SELECT 'trigger ' || pg_get_triggerdef(oid) FROM pg_trigger WHERE NOT tgisinternal
		UNION ALL
		SELECT 'function ' || pg_get_functiondef(oid) FROM pg_proc WHERE pronamespace = 'public'::regnamespace
		UNION ALL
		SELECT 'view ' || viewname || ' ' || definition FROM pg_views WHERE schemaname = 'public'
		ORDER BY 1`)
	Expect(err).NotTo(HaveOccurred())
	defer rows.Close()
	var objects []string
	for rows.Next() {
		var object string
		Expect(rows.Scan(&object)).To(Succeed())
		objects = append(objects, object)
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	return objects
}

func countRelations(ctx context.Context, db *pgxpool.Pool) int {
	GinkgoHelper()
	var n int
	Expect(db.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'`).Scan(&n)).To(Succeed())
	return n
}

// nodeKey identifies a node the way the Keys section of the domain model
// does: by its project and its number within it.
type nodeKey struct {
	project string
	number  int64
}

// standIn is a minimal nodes table created in the spec's own database. The
// real one arrives with the nodes migration; what this suite proves is that
// the project half of a reference keeps resolving across a rename.
type standIn struct{ db *pgxpool.Pool }

func standInNodes(ctx context.Context, db *pgxpool.Pool) standIn {
	GinkgoHelper()
	_, err := db.Exec(ctx, `
		CREATE TABLE spec_nodes (
			project_id text   NOT NULL REFERENCES projects (id),
			number     bigint NOT NULL,
			PRIMARY KEY (project_id, number)
		)`)
	Expect(err).NotTo(HaveOccurred())
	return standIn{db: db}
}

func (s standIn) insert(ctx context.Context, project string, number int64) {
	GinkgoHelper()
	_, err := s.db.Exec(ctx, `INSERT INTO spec_nodes (project_id, number) VALUES ($1, $2)`, project, number)
	Expect(err).NotTo(HaveOccurred())
}

// resolve turns a `slug#number` reference into the node it names, going
// through project_slugs so retired slugs count.
func (s standIn) resolve(ctx context.Context, orgID, ref string) nodeKey {
	GinkgoHelper()
	slug, numberText, ok := strings.Cut(ref, "#")
	Expect(ok).To(BeTrue(), "reference %q has no number", ref)
	number, err := strconv.ParseInt(numberText, 10, 64)
	Expect(err).NotTo(HaveOccurred())

	var key nodeKey
	Expect(s.db.QueryRow(ctx, `
		SELECT n.project_id, n.number
		  FROM project_slugs p
		  JOIN spec_nodes n ON n.project_id = p.project_id
		 WHERE p.org_id = $1 AND p.slug = $2 AND n.number = $3`, orgID, slug, number).Scan(&key.project, &key.number)).
		To(Succeed(), fmt.Sprintf("resolve %q", ref))
	return key
}
