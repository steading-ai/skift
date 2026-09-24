package domain_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
)

// Readiness is one rule stated in three places: the ready query negates
// every condition, the derived flags select every condition un-negated,
// and Explain runs each one to say why. Nothing in the compiler connects
// them. The property specs catch a rule that is composed wrongly; this
// catches a condition added to one statement and forgotten in the other,
// without needing a database to do it.
var _ = Describe("Readiness conditions", Label("unit"), func() {
	It("Every condition is negated by the ready query", func() {
		By("given the readiness fragments")
		By("then each appears inside the ready predicate")
		for name, fragment := range domain.ReadyFragments {
			Expect(domain.ReadyWhere).To(ContainSubstring(fragment), "the ready predicate is missing the %s condition", name)
		}
		By("and the predicate negates those and nothing else")
		Expect(strings.Count(domain.ReadyWhere, "NOT EXISTS (")).To(Equal(len(domain.ReadyFragments)))
	})

	It("The derived flags answer readiness with the ready query's own predicate", func() {
		By("given the readiness fragments")
		By("then each appears in the statement behind the derived flags")
		for name, fragment := range domain.ReadyFragments {
			Expect(domain.DerivedSQL).To(ContainSubstring(fragment), "the derived flags are missing the %s condition", name)
		}
		By("and the ready flag is the ready predicate itself, not a second statement of it")
		Expect(domain.DerivedSQL).To(ContainSubstring(domain.ReadyWhere))
	})
})
