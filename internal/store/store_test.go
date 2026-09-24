package store_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// query is one named statement from internal/store/queries, as sqlc sees it.
type query struct {
	file, name, sql string
}

var (
	queryHeader = regexp.MustCompile(`(?m)^-- name: (\w+) :\w+\s*$`)
	// An organization predicate as sqlc's static SQL states it: the column
	// compared for equality against a positional parameter. Anything
	// looser, such as a nullable argument or an OR, would let a caller
	// widen the scope.
	orgPredicate = regexp.MustCompile(`\b(?:\w+\.)?org_id\s*=\s*\$\d+`)
	// An INSERT scopes its row through the org_id column it writes.
	orgColumn = regexp.MustCompile(`(?i)INSERT INTO \w+ \([^)]*\borg_id\b[^)]*\)`)
	// The tables the isolation boundary is about.
	tenantTables = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|INTO|UPDATE)\s+(nodes|edges|events|comments)\b`)
)

// loadQueries parses every query file the way sqlc does: a `-- name:`
// header, then the statement up to the next header.
func loadQueries() []query {
	GinkgoHelper()
	files, err := filepath.Glob(filepath.Join("queries", "*.sql"))
	Expect(err).NotTo(HaveOccurred())
	Expect(files).NotTo(BeEmpty())

	var queries []query
	for _, file := range files {
		content, err := os.ReadFile(file)
		Expect(err).NotTo(HaveOccurred())
		text := string(content)
		headers := queryHeader.FindAllStringSubmatchIndex(text, -1)
		for i, h := range headers {
			end := len(text)
			if i+1 < len(headers) {
				end = headers[i+1][0]
			}
			queries = append(queries, query{
				file: filepath.Base(file),
				name: text[h[2]:h[3]],
				sql:  text[h[1]:end],
			})
		}
	}
	return queries
}

// isOrgScoped reports whether q pins its rows to one organization in
// static SQL.
func isOrgScoped(q query) bool {
	if strings.HasPrefix(strings.TrimSpace(q.sql), "INSERT") && orgColumn.MatchString(q.sql) {
		return true
	}
	return orgPredicate.MatchString(q.sql)
}

var _ = Describe("Generated query set", Label("unit"), func() {
	Describe("Scenarios", func() {
		It("Organization scoping is not optional", func() {
			By("given the generated query set")
			queries := loadQueries()

			By("when I inspect every query returning nodes, edges, events, or comments")
			var inspected []string
			for _, q := range queries {
				if !tenantTables.MatchString(q.sql) {
					continue
				}
				inspected = append(inspected, q.name)
				By("then " + q.name + " contains an org_id predicate in its static SQL")
				Expect(isOrgScoped(q)).To(BeTrue(), "%s (%s) has no static org_id predicate:\n%s", q.name, q.file, q.sql)
			}
			Expect(inspected).To(ContainElements("GetNode", "ListNodes", "CreateEdge", "ListEventsAfter", "ListCommentsForNode"))
		})
	})

	It("scopes every query to one organization, apart from the tenancy root and identity lookup", func() {
		// The organizations table is the scope itself, and an identity is
		// how a transport learns which organization it is talking to.
		unscoped := map[string]bool{"CreateOrganization": true, "GetOrganization": true, "GetOrganizationByName": true, "ListOrganizations": true, "GetPrincipalByIdentity": true}
		for _, q := range loadQueries() {
			if unscoped[q.name] {
				continue
			}
			Expect(isOrgScoped(q)).To(BeTrue(), "%s (%s) has no static org_id predicate:\n%s", q.name, q.file, q.sql)
		}
	})

	It("never lets a nullable or optional organization argument widen a query", func() {
		for _, q := range loadQueries() {
			Expect(q.sql).NotTo(MatchRegexp(`(?i)org_id\s+IS\s+NULL`), q.name)
			Expect(q.sql).NotTo(MatchRegexp(`(?i)sqlc\.narg\(\s*org_id\s*\)`), q.name)
		}
	})
})

// newULID returns a random identifier in the canonical ULID text form.
func newULID() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b [26]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[0] = alphabet[b[0]%8]
	for i := 1; i < len(b); i++ {
		b[i] = alphabet[b[i]%32]
	}
	return string(b[:])
}

var _ = Describe("Store", Label(testsupport.DBLabel), func() {
	var s *store.Store

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
	})

	// createOrg inserts an organization inside tx and returns its id.
	createOrg := func(ctx context.Context, tx store.Tx) string {
		GinkgoHelper()
		org, err := tx.CreateOrganization(ctx, store.CreateOrganizationParams{ID: newULID(), Name: "acme"})
		Expect(err).NotTo(HaveOccurred())
		return org.ID
	}

	Describe("WithTx", func() {
		It("commits when the operation returns nil", func(ctx SpecContext) {
			var id string
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				id = createOrg(ctx, tx)
				return nil
			})).To(Succeed())

			org, err := s.Queries().GetOrganization(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(org.Name).To(Equal("acme"))
		})

		It("rolls back when the operation returns an error, and returns that error", func(ctx SpecContext) {
			boom := errors.New("boom")
			var id string
			err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				id = createOrg(ctx, tx)
				return boom
			})
			Expect(err).To(MatchError(boom))

			_, err = s.Queries().GetOrganization(ctx, id)
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})

		It("rolls back when the operation panics, and re-panics", func(ctx SpecContext) {
			var id string
			Expect(func() {
				_ = s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
					id = createOrg(ctx, tx)
					panic("boom")
				})
			}).To(PanicWith("boom"))

			_, err := s.Queries().GetOrganization(ctx, id)
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})

		It("runs every statement of one operation in the same transaction", func(ctx SpecContext) {
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				id := createOrg(ctx, tx)

				By("the generated queries see the uncommitted write")
				org, err := tx.GetOrganization(ctx, id)
				Expect(err).NotTo(HaveOccurred())
				Expect(org.ID).To(Equal(id))

				By("and so does the raw transaction, for statements the query set lacks")
				var txid int64
				Expect(tx.Tx.QueryRow(ctx, `SELECT txid_current()`).Scan(&txid)).To(Succeed())
				Expect(txid).NotTo(BeZero())

				By("while a connection outside the transaction does not")
				_, err = s.Queries().GetOrganization(ctx, id)
				Expect(err).To(MatchError(pgx.ErrNoRows))
				return nil
			})).To(Succeed())
		})
	})

	Describe("Statement timeout", func() {
		It("is in force by default and set locally to each transaction", func(ctx SpecContext) {
			Expect(s.StatementTimeout()).To(Equal(store.DefaultStatementTimeout))
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				var timeout string
				Expect(tx.Tx.QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout)).To(Succeed())
				Expect(timeout).To(Equal("30s"))
				return nil
			})).To(Succeed())

			By("and a connection outside a transaction carries none over")
			var timeout string
			Expect(s.Pool().QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout)).To(Succeed())
			Expect(timeout).To(Equal("0"))
		})

		It("cuts off a pathological recursive query rather than stalling", func(ctx SpecContext) {
			s.SetStatementTimeout(200 * time.Millisecond)
			start := time.Now()
			err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				var n int64
				return tx.Tx.QueryRow(ctx, `
					WITH RECURSIVE forever(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM forever)
					SELECT count(*) FROM forever`).Scan(&n)
			})
			Expect(time.Since(start)).To(BeNumerically("<", 10*time.Second))
			var pgErr *pgconn.PgError
			Expect(errors.As(err, &pgErr)).To(BeTrue(), "expected a PostgreSQL error, got %v", err)
			Expect(pgErr.Code).To(Equal("57014"), "query_canceled")

			By("and the store answers the next transaction")
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				var one int
				return tx.Tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
			})).To(Succeed())
		})
	})

	Describe("Organization scoping", func() {
		It("does not return one organization's node to another", func(ctx SpecContext) {
			var first, second, nodeID string
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				first = createOrg(ctx, tx)
				second = createOrg(ctx, tx)
				project, err := tx.CreateProject(ctx, store.CreateProjectParams{ID: newULID(), OrgID: first, Slug: "platform", Title: "Platform", Priority: "p2"})
				Expect(err).NotTo(HaveOccurred())
				node, err := tx.CreateNode(ctx, store.CreateNodeParams{
					ID: newULID(), Number: 1, OrgID: first, ProjectID: project.ID,
					Kind: "task", Title: "Node", Priority: "p2", Labels: []string{},
				})
				Expect(err).NotTo(HaveOccurred())
				nodeID = node.ID
				return nil
			})).To(Succeed())

			q := s.Queries()
			_, err := q.GetNode(ctx, store.GetNodeParams{OrgID: first, ID: nodeID})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.GetNode(ctx, store.GetNodeParams{OrgID: second, ID: nodeID})
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})
	})
})
