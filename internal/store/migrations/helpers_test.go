package migrations_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
)

// PostgreSQL error codes the specs distinguish.
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	uniqueViolation = "23505"
	checkViolation  = "23514"
)

// newULID returns a random identifier in the canonical ULID text form. The
// specs need ids the schema accepts, not ids that sort by time.
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

func newOrg(ctx context.Context, db *pgxpool.Pool) string {
	GinkgoHelper()
	id := newULID()
	_, err := db.Exec(ctx, `INSERT INTO organizations (id, name) VALUES ($1, $2)`, id, "org "+id)
	Expect(err).NotTo(HaveOccurred())
	return id
}

func newProject(ctx context.Context, db *pgxpool.Pool, orgID, slug string) string {
	GinkgoHelper()
	id := newULID()
	Expect(insertProject(ctx, db, id, orgID, slug)).To(Succeed())
	return id
}

func insertProject(ctx context.Context, db *pgxpool.Pool, id, orgID, slug string) error {
	_, err := db.Exec(ctx, `INSERT INTO projects (id, org_id, slug, title) VALUES ($1, $2, $3, $4)`, id, orgID, slug, "Project "+slug)
	return err
}

func newPrincipal(ctx context.Context, db *pgxpool.Pool, orgID, kind string) string {
	GinkgoHelper()
	id := newULID()
	_, err := db.Exec(ctx, `INSERT INTO principals (id, org_id, kind) VALUES ($1, $2, $3)`, id, orgID, kind)
	Expect(err).NotTo(HaveOccurred())
	return id
}

func addIdentity(ctx context.Context, db *pgxpool.Pool, principalID, scheme, value string) {
	GinkgoHelper()
	_, err := db.Exec(ctx, `INSERT INTO principal_identities (principal_id, scheme, value) VALUES ($1, $2, $3)`, principalID, scheme, value)
	Expect(err).NotTo(HaveOccurred())
}

// resolveIdentity answers "which principal is this?" the way a transport
// would, from the scheme and value it observed.
func resolveIdentity(ctx context.Context, db *pgxpool.Pool, scheme, value string) string {
	GinkgoHelper()
	var id string
	Expect(db.QueryRow(ctx, `SELECT principal_id FROM principal_identities WHERE scheme = $1 AND value = $2`, scheme, value).Scan(&id)).To(Succeed())
	return id
}

// resolveSlug finds the project a slug refers to within an organization,
// whether the slug is live or retired.
func resolveSlug(ctx context.Context, db *pgxpool.Pool, orgID, slug string) (projectID string, retired bool) {
	GinkgoHelper()
	var retiredAt *string
	Expect(db.QueryRow(ctx, `SELECT project_id, retired_at::text FROM project_slugs WHERE org_id = $1 AND slug = $2`, orgID, slug).Scan(&projectID, &retiredAt)).To(Succeed())
	return projectID, retiredAt != nil
}

// HavePgCode matches an error whose PostgreSQL SQLSTATE is code.
func HavePgCode(code string) types.GomegaMatcher {
	return WithTransform(func(err error) string {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgErr.Code
		}
		return "not a PostgreSQL error: " + err.Error()
	}, Equal(code))
}

// repeat builds a slug of exactly n characters.
func repeat(n int) string {
	return strings.Repeat("a", n)
}
