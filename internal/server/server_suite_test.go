package server_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/testsupport"
)

func TestServer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "server")
}

// pg is the server the db-labelled specs clone their databases from. The
// template carries the current schema, since the handler specs drive the
// real domain over the wire.
var pg *testsupport.Postgres

// pol is the default policy bundle every write in the handler specs is
// authorized by.
var pol *policy.Bundle

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
	started, err := testsupport.Start(ctx, domain.Migrate)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(started.Stop)
	return []byte(started.URL())
}, func(ctx context.Context, adminURL []byte) {
	var err error
	pol, err = policy.LoadDefault(ctx)
	Expect(err).NotTo(HaveOccurred())
	if len(adminURL) == 0 {
		return
	}
	pg, err = testsupport.Attach(ctx, string(adminURL))
	Expect(err).NotTo(HaveOccurred())
})
