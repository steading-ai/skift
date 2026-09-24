package domain_test

import (
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
)

// Ids order rows: a patch's nodes list in creation order because their
// ids do. These specs hold the generator to that without a database.
var _ = Describe("Id generation", Label("unit"), func() {
	It("mints ids that sort in the order they were minted, however fast", func() {
		ids := make([]string, 0, 2000)
		for range cap(ids) {
			ids = append(ids, domain.NewULID())
		}
		Expect(sort.StringsAreSorted(ids)).To(BeTrue(), "ids minted within one millisecond count up rather than draw again")
		Expect(ids[0]).NotTo(Equal(ids[1]))
		Expect(ids[0]).To(HaveLen(26))
	})

	It("carries the clock forward, so an id minted later reads as later", func() {
		earlier := domain.NewULID()
		time.Sleep(3 * time.Millisecond)
		later := domain.NewULID()
		Expect(later[:10]).NotTo(Equal(earlier[:10]), "the first ten characters are the millisecond")
		Expect(later > earlier).To(BeTrue())
	})

	It("encodes the millisecond it was minted in its first ten characters", func() {
		const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
		before := time.Now().UnixMilli()
		id := domain.NewULID()
		var ms int64
		for _, c := range id[:10] {
			ms = ms<<5 | int64(strings.IndexRune(crockford, c))
		}
		Expect(ms).To(BeNumerically(">=", before))
		Expect(ms).To(BeNumerically("<=", time.Now().UnixMilli()))
	})
})
