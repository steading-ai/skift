package cli_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/testsupport"
)

func TestCLI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "cli")
}

// pg is the PostgreSQL the db-labelled specs run serve against, attached in
// every process; nil when the label filter admits none of them.
var pg *testsupport.Postgres

// dbSpecsSelected reports whether the label filter admits any db-labelled
// spec. This suite is mostly unit specs, so it must leave podman alone
// under `make test-unit`.
func dbSpecsSelected() bool {
	return Label(testsupport.DBLabel).MatchesLabelFilter(GinkgoLabelFilter())
}

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	if !dbSpecsSelected() {
		return nil
	}
	started, err := testsupport.Start(ctx, admin.Migrate)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(started.Stop)
	return []byte(started.URL())
}, func(ctx context.Context, handoff []byte) {
	if len(handoff) == 0 {
		return
	}
	var err error
	pg, err = testsupport.Attach(ctx, string(handoff))
	Expect(err).NotTo(HaveOccurred())
})
