package main_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/testsupport"
)

func TestDg(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "dg binary")
}

// pg is the server the db-labelled specs clone their databases from, for
// the faces of the binary that reach the database themselves: serve and
// reap.
var pg *testsupport.Postgres

// dbSpecsSelected reports whether the label filter admits any db-labelled
// spec. This suite mixes unit and db specs, so it must leave podman alone
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
	if len(adminURL) == 0 {
		return
	}
	var err error
	pg, err = testsupport.Attach(ctx, string(adminURL))
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	gexec.CleanupBuildArtifacts()
})
