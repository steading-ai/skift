package domain_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
)

// The replay window is the oldest monthly partition still kept, so its
// start is a month boundary in the past, ReplayPartitions-1 months back.
// The watch specs age cursors relative to this function, so this one
// pins the arithmetic itself.
var _ = Describe("The replay window", Label("unit"), func() {
	It("starts on the first of the month, ReplayPartitions-1 months before now", func() {
		now := time.Date(2026, time.September, 24, 11, 37, 0, 0, time.UTC)
		start := domain.ReplayWindowStart(now)
		Expect(start).To(BeTemporally("==", time.Date(2026, time.September-(domain.ReplayPartitions-1), 1, 0, 0, 0, 0, time.UTC)))
		Expect(start).To(BeTemporally("<", now))
	})

	It("counts back across a year boundary", func() {
		start := domain.ReplayWindowStart(time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC))
		Expect(start).To(BeTemporally("==", time.Date(2027, time.January-(domain.ReplayPartitions-1), 1, 0, 0, 0, 0, time.UTC)))
	})

	It("reads the moment in UTC, so a local midnight does not move the boundary", func() {
		east := time.FixedZone("east", 10*3600)
		local := time.Date(2026, time.October, 1, 3, 0, 0, 0, east) // still September 30 in UTC
		Expect(domain.ReplayWindowStart(local)).To(BeTemporally("==", domain.ReplayWindowStart(local.UTC())))
		Expect(domain.ReplayWindowStart(local).Month()).To(Equal(time.September - (domain.ReplayPartitions - 1)))
	})
})
