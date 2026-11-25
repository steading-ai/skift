package cli_test

import (
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/sdnotify"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("Events partitions at serve start", Label(testsupport.DBLabel), func() {
	It("serve starting against a database whose partitions end in the past creates the missing ones before reporting ready", func(ctx SpecContext) {
		db := pg.Database(ctx)
		month := time.Now().UTC()
		month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)

		By("given a database with no events partition for this month or any later one")
		rows, err := db.Query(ctx, `
			SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			 WHERE i.inhparent = 'events'::regclass AND c.relname >= $1`, "events_"+month.Format("200601"))
		Expect(err).NotTo(HaveOccurred())
		current, err := pgx.CollectRows(rows, pgx.RowTo[string])
		Expect(err).NotTo(HaveOccurred())
		Expect(current).NotTo(BeEmpty())
		for _, name := range current {
			_, err := db.Exec(ctx, `DROP TABLE `+pgx.Identifier{name}.Sanitize())
			Expect(err).NotTo(HaveOccurred())
		}

		By("when serve starts and tells systemd it is ready")
		queued := notifyListener()
		DeferCleanup(startServe(db.Config().ConnString()))
		Eventually(func() string {
			for {
				state, err := queued()
				if err != nil {
					return ""
				}
				if strings.Contains(state, sdnotify.Ready) {
					return state
				}
			}
		}, 30*time.Second).ShouldNot(BeEmpty(), "serve never reported ready")

		By("then this month's partition and the horizon after it were already there")
		rows, err = db.Query(ctx, `
			SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			 WHERE i.inhparent = 'events'::regclass`)
		Expect(err).NotTo(HaveOccurred())
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		Expect(err).NotTo(HaveOccurred())
		for i := 0; i <= admin.EventPartitionsAhead; i++ {
			Expect(names).To(ContainElement("events_" + month.AddDate(0, i, 0).Format("200601")))
		}
	})
})
