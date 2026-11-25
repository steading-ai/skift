package migrations_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/store/migrations"
	"github.com/steading-ai/skift/internal/testsupport"
)

// Most of this suite runs against a real PostgreSQL, but the check that
// shipped migrations never change needs none, so the suite must not start a
// container when `make test-unit` filters every db spec out. The preview
// below asks Ginkgo which specs this run will execute before any of them
// runs.
var needsDatabase bool

func TestMigrations(t *testing.T) {
	RegisterFailHandler(Fail)
	needsDatabase = previewNeedsDatabase()
	RunSpecs(t, "migrations")
}

// pg is the server every spec in this suite clones from. Its template
// carries the full schema, applied by the migrations under test.
var pg *testsupport.Postgres

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	if !needsDatabase {
		return nil
	}
	started, err := testsupport.Start(ctx, migrations.Migrate)
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

// previewNeedsDatabase reports whether the current filters leave any spec
// carrying the db label to run.
func previewNeedsDatabase() bool {
	for _, spec := range PreviewSpecs("migrations").SpecReports {
		if spec.LeafNodeType != types.NodeTypeIt || spec.State == types.SpecStateSkipped {
			continue
		}
		for _, label := range spec.Labels() {
			if label == testsupport.DBLabel {
				return true
			}
		}
	}
	return false
}
