package admin_test

import (
	"context"
	"crypto/rand"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("Keeping events partitions ahead of the clock", Label(testsupport.DBLabel), func() {
	var (
		db   *pgxpool.Pool
		s    *store.Store
		org  string
		user string
		log  *slog.Logger
	)

	BeforeEach(func(ctx SpecContext) {
		db = pg.Database(ctx)
		s = store.FromPool(db)
		out, err := admin.Bootstrap(ctx, s, admin.BootstrapParams{OrgName: "acme", UID: 1000})
		Expect(err).NotTo(HaveOccurred())
		org, user = out.Organization.ID, out.Principal.ID
		log = slog.New(slog.NewTextHandler(GinkgoWriter, nil))
	})

	// writeEventAt records an event as having happened at t.
	writeEventAt := func(ctx context.Context, t time.Time) error {
		return s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := store.Raw(tx).Exec(ctx, `
				INSERT INTO events (id, org_id, principal_id, principal_kind, identity_scheme, policy_version, op, occurred_at)
				VALUES ($1, $2, $3, 'user', 'unix', 'sha256:spec', 'spec', $4)`,
				newULID(), org, user, t)
			return err
		})
	}

	It("A write past the last partition succeeds once the upkeep has run", func(ctx SpecContext) {
		By("given a database whose newest events partition is for some month")
		names := eventPartitions(ctx, db)
		Expect(names).NotTo(BeEmpty())
		last, err := time.Parse("200601", names[len(names)-1][len("events_"):])
		Expect(err).NotTo(HaveOccurred())
		past := last.AddDate(0, 1, 0).Add(time.Hour)

		By("when the clock is in the month after it, a write there has nowhere to go")
		Expect(writeEventAt(ctx, past)).To(MatchError(ContainSubstring("no partition of relation")))

		By("and the upkeep runs once at that time")
		Expect(admin.EnsureEventPartitions(ctx, s, log, past)).To(Succeed())

		By("then the write succeeds, and the partitions reach the horizon past it")
		Expect(writeEventAt(ctx, past)).To(Succeed())
		for i := 0; i <= admin.EventPartitionsAhead; i++ {
			Expect(eventPartitions(ctx, db)).To(ContainElement("events_" + past.AddDate(0, i, 0).UTC().Format("200601")))
		}
	})

	It("Running the upkeep twice creates nothing the second time", func(ctx SpecContext) {
		later := time.Now().AddDate(0, 6, 0)
		created, err := s.EnsureEventPartitions(ctx, later, admin.EventPartitionsAhead)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).NotTo(BeEmpty())

		again, err := s.EnsureEventPartitions(ctx, later, admin.EventPartitionsAhead)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(BeEmpty())
	})
})

// eventPartitions names the partitions of events, oldest first.
func eventPartitions(ctx context.Context, db *pgxpool.Pool) []string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `
		SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		 WHERE i.inhparent = 'events'::regclass ORDER BY 1`)
	Expect(err).NotTo(HaveOccurred())
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return names
}

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
