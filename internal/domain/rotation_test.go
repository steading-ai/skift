package domain_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
)

// NextProject is the cursor arithmetic behind the round-robin turn across
// projects: it never touches a database, so these specs check it directly.
var _ = Describe("Round-robin project rotation", Label("unit"), func() {
	It("starts at the lowest id when there is no cursor", func() {
		Expect(domain.NextProject(nil, []string{"b", "a", "c"})).To(Equal("a"))
	})

	It("advances to the next id in sorted order", func() {
		Expect(domain.NextProject(ptr("a"), []string{"a", "b", "c"})).To(Equal("b"))
	})

	It("wraps from the highest id back to the lowest", func() {
		Expect(domain.NextProject(ptr("c"), []string{"a", "b", "c"})).To(Equal("a"))
	})

	It("restarts the cycle when the cursor's project is no longer eligible", func() {
		Expect(domain.NextProject(ptr("dropped"), []string{"a", "b", "c"})).To(Equal("a"))
	})

	It("stays put across turns when only one project is eligible", func() {
		Expect(domain.NextProject(ptr("a"), []string{"a"})).To(Equal("a"))
		Expect(domain.NextProject(nil, []string{"a"})).To(Equal("a"))
	})
})

func ptr(s string) *string { return &s }
