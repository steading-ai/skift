package migrations_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/steading-ai/skift/internal/store/migrations"
	"github.com/steading-ai/skift/internal/testsupport"
)

// Most of this suite runs against a real PostgreSQL, but the check that
// shipped migrations never change needs none, so the suite must not start a
// container when `make test-unit` filters every db spec out. The preview
// below asks Ginkgo which specs this run will execute before any of them
// runs. The downgrade specs also need this tree's binary, which takes a
// build, so the suite builds it only when one of them runs.
var needsDatabase, needsBinary bool

func TestMigrations(t *testing.T) {
	RegisterFailHandler(Fail)
	needsDatabase = previewNeeds(testsupport.DBLabel)
	needsBinary = previewNeeds(downgradeLabel)
	RunSpecs(t, "migrations")
}

// pg is the server every spec in this suite clones from. Its template
// carries the full schema, applied by the migrations under test.
var pg *testsupport.Postgres

// thisTree is the skift built from this tree, when a downgrade spec runs.
var thisTree string

// suiteState is what the first process hands the others.
type suiteState struct {
	AdminURL string `json:"admin_url"`
	Binary   string `json:"binary"`
}

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	var state suiteState
	if needsDatabase {
		started, err := testsupport.Start(ctx, migrations.Migrate)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(started.Stop)
		state.AdminURL = started.URL()
	}
	if needsBinary {
		bin, err := gexec.Build("github.com/steading-ai/skift/cmd/skift")
		Expect(err).NotTo(HaveOccurred())
		state.Binary = bin
	}
	raw, err := json.Marshal(state)
	Expect(err).NotTo(HaveOccurred())
	return raw
}, func(ctx context.Context, raw []byte) {
	var state suiteState
	Expect(json.Unmarshal(raw, &state)).To(Succeed())
	thisTree = state.Binary
	if state.AdminURL == "" {
		return
	}
	var err error
	pg, err = testsupport.Attach(ctx, state.AdminURL)
	Expect(err).NotTo(HaveOccurred())
})

var _ = SynchronizedAfterSuite(func() {}, func() {
	gexec.CleanupBuildArtifacts()
})

// previewNeeds reports whether the current filters leave any spec
// carrying label to run.
func previewNeeds(label string) bool {
	for _, spec := range PreviewSpecs("migrations").SpecReports {
		if spec.LeafNodeType != types.NodeTypeIt || spec.State == types.SpecStateSkipped {
			continue
		}
		if slices.Contains(spec.Labels(), label) {
			return true
		}
	}
	return false
}
