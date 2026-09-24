package migrations_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("Change stream", Label(testsupport.DBLabel), func() {
	It("records the writing transaction on every event", func(ctx SpecContext) {
		db := pg.Database(ctx)
		f := newFixture(ctx, db)
		id := f.event(ctx, "create_node")

		var txID int64
		Expect(db.QueryRow(ctx, `SELECT tx_id FROM events WHERE id = $1`, id).Scan(&txID)).To(Succeed())
		Expect(txID).To(BeNumerically(">", 0))
	})

	It("notifies listeners of the organization an event was committed in", func(ctx SpecContext) {
		db := pg.Database(ctx)
		f := newFixture(ctx, db)
		conn, err := db.Acquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Release()
		_, err = conn.Exec(ctx, `LISTEN skift_events`)
		Expect(err).NotTo(HaveOccurred())

		f.event(ctx, "create_node")

		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := conn.Conn().WaitForNotification(waitCtx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Channel).To(Equal("skift_events"))
		Expect(n.Payload).To(Equal(f.org))
	})
})
