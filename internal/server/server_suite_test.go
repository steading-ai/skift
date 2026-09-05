package server_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/testsupport"
)

func TestServer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "server")
}

// pg is the server the db-labelled specs use to prove the health check
// against a real database. Its template needs no schema: the check only
// pings.
var pg *testsupport.Postgres

// dbSpecsSelected reports whether the label filter admits any db-labelled
// spec. This suite mixes unit and db specs, so unlike a db-only suite it
// cannot rely on Ginkgo skipping suite setup: it must leave podman alone
// itself under `make test-unit`.
func dbSpecsSelected() bool {
	return Label(testsupport.DBLabel).MatchesLabelFilter(GinkgoLabelFilter())
}

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	if !dbSpecsSelected() {
		return nil
	}
	started, err := testsupport.Start(ctx, nil)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(started.Stop)
	return []byte(started.URL())
}, func(ctx context.Context, adminURL []byte) {
	if len(adminURL) == 0 {
		return
	}
	var err error
	pg, err = testsupport.Attach(ctx, string(adminURL))
	Expect(err).NotTo(HaveOccurred())
})
