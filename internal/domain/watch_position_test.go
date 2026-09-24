package domain_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
)

// A watch resumes from its cursor exactly once: every event of a
// transaction below the boundary was delivered, and every delivered
// event at or above it is listed as seen. These specs hold the arithmetic
// that moves the boundary and the seen set to that, page by page.
var _ = Describe("The watch position", Label("unit"), func() {
	at := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	ev := func(tx int64, id string) store.Event { return store.Event{TxID: tx, ID: id} }
	seen := func(tx int64, id string) domain.SeenEvent { return domain.SeenEvent{TxID: tx, ID: id} }

	It("takes the read's boundary after a short page, keeping only what sits at or above it", func() {
		from := domain.Position{XMin: 9, Seen: []domain.SeenEvent{seen(9, "z"), seen(10, "y")}}
		p := domain.CursorAfter(from, 3, 10, at, []store.Event{ev(8, "a"), ev(9, "b")}, 2)
		Expect(p.XMin).To(Equal(int64(10)), "the page was short, so everything below the read's boundary is delivered")
		Expect(p.Seen).To(Equal([]domain.SeenEvent{seen(10, "y")}), "a seen event exactly at the boundary stays; the delivered ones are below it")
		Expect(p.At).To(Equal(at))
	})

	It("stops at the last row's transaction after a full page, since the rest of it may be unread", func() {
		from := domain.Position{XMin: 10, Seen: []domain.SeenEvent{seen(14, "q"), seen(15, "p")}}
		p := domain.CursorAfter(from, 3, 20, at, []store.Event{ev(12, "a"), ev(15, "b"), ev(15, "c")}, 3)
		Expect(p.XMin).To(Equal(int64(15)))
		Expect(p.Seen).To(Equal([]domain.SeenEvent{seen(15, "p"), seen(15, "b"), seen(15, "c")}), "events at exactly the boundary must be listed, or they replay")
	})

	It("stops at the last consumed row's transaction when the page was only partly consumed", func() {
		p := domain.CursorAfter(domain.Position{XMin: 10}, 3, 20, at, []store.Event{ev(12, "a"), ev(15, "b"), ev(18, "c")}, 2)
		Expect(p.XMin).To(Equal(int64(15)))
		Expect(p.Seen).To(Equal([]domain.SeenEvent{seen(15, "b")}), "c was not consumed, so it is not seen")
	})

	It("moves to the read's boundary and keeps its seen set when nothing was consumed", func() {
		from := domain.Position{XMin: 10, Seen: []domain.SeenEvent{seen(20, "p")}}
		p := domain.CursorAfter(from, 3, 20, at, []store.Event{ev(15, "a")}, 0)
		Expect(p.XMin).To(Equal(int64(20)))
		Expect(p.Seen).To(Equal([]domain.SeenEvent{seen(20, "p")}))
	})

	It("does not move the boundary past the read's after a full page above it", func() {
		p := domain.CursorAfter(domain.Position{XMin: 10}, 3, 20, at, []store.Event{ev(21, "a"), ev(22, "b"), ev(23, "c")}, 3)
		Expect(p.XMin).To(Equal(int64(20)))
		Expect(p.Seen).To(Equal([]domain.SeenEvent{seen(21, "a"), seen(22, "b"), seen(23, "c")}))
	})
})
