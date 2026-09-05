package domain_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
)

var _ = Describe("Project slugs", Label("unit"), func() {
	DescribeTable("derives a slug from a title",
		func(title, slug string) { Expect(domain.Slugify(title)).To(Equal(slug)) },
		Entry("lowercases and hyphenates", "OAuth Spike!", "oauth-spike"),
		Entry("folds runs of punctuation", "a -- b__c", "a-b-c"),
		Entry("keeps letters of any script", "Übersicht Ω", "übersicht-ω"),
		Entry("falls back when nothing is left", "!!!", "project"),
		Entry("bounds the length", strings.Repeat("x", 120), strings.Repeat("x", 100)),
	)

	DescribeTable("normalizes a requested slug with the least change",
		func(requested, slug string) { Expect(domain.NormalizeSlug(requested)).To(Equal(slug)) },
		Entry("lowercases", "OAuth_Spike", "oauth_spike"),
		Entry("removes the reference delimiters", "My Slug/Here#1", "my-slug-here-1"),
		Entry("keeps other punctuation", "spike.v2", "spike.v2"),
	)
})
