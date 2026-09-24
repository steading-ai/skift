package domain_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
)

var _ = Describe("Reason", Label("unit"), func() {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	held := "not this quarter"

	DescribeTable("says why a node is not ready, in one clause a person can read",
		func(r domain.Reason, text string) { Expect(r.String()).To(Equal(text)) },
		Entry("not open", domain.Reason{Kind: domain.ReasonNotOpen, Status: "done"}, "it is done"),
		Entry("archived", domain.Reason{Kind: domain.ReasonArchived, Project: &store.Project{Slug: "platform"}}, "its project platform is archived"),
		Entry("blocked by an open node", domain.Reason{Kind: domain.ReasonBlocked, Blocker: &store.Node{Number: 3, Status: "open"}}, "it is blocked by open node #3"),
		Entry("blocked by a gate", domain.Reason{Kind: domain.ReasonBlocked, Gate: true, Blocker: &store.Node{Number: 3, Status: "dropped"}}, "it is blocked by dropped gate #3"),
		Entry("leased", domain.Reason{Kind: domain.ReasonLeased, Lease: &store.Lease{ID: "01JL", ExpiresAt: at}}, "it is held by lease 01JL until 2026-09-01T12:00:00Z"),
		Entry("open child", domain.Reason{Kind: domain.ReasonOpenChild, Child: &store.Node{Number: 4}}, "its child #4 is open"),
		Entry("held itself", domain.Reason{Kind: domain.ReasonHeld, Hold: &domain.Hold{Self: true, At: at, Reason: "parked"}}, "it is held since 2026-09-01T12:00:00Z: parked"),
		Entry("held through an ancestor", domain.Reason{Kind: domain.ReasonHeld, Hold: &domain.Hold{Number: 2, At: at, Reason: "parked"}}, "it is contained in #2, which is held since 2026-09-01T12:00:00Z: parked"),
		Entry("project held", domain.Reason{Kind: domain.ReasonProjectHeld, Project: &store.Project{Slug: "platform", HeldReason: &held}}, "its project platform is held: not this quarter"),
		Entry("a kind it does not know", domain.Reason{Kind: "eclipsed"}, "eclipsed"),
	)
})
