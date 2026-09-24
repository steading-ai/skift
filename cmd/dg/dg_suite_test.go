package main_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

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

// specVersion is the version the suite's binary is stamped with, in the
// same link step the Makefile performs.
const specVersion = "1.2.3-spec+abc123"

// binary is the dg every spec runs, linked once for the suite. A freshly
// linked binary is slow the first time it starts, and a link per spec paid
// that inside every spec's wait.
var binary string

// suiteState is what the first process hands the others.
type suiteState struct {
	AdminURL string `json:"admin_url"`
	Binary   string `json:"binary"`
}

// dbSpecsSelected reports whether the label filter admits any db-labelled
// spec. This suite mixes unit and db specs, so it must leave podman alone
// itself under `make test-unit`.
func dbSpecsSelected() bool {
	return Label(testsupport.DBLabel).MatchesLabelFilter(GinkgoLabelFilter())
}

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	bin, err := gexec.Build("github.com/ivy/dagger/cmd/dg",
		"-ldflags", "-X github.com/ivy/dagger/internal/version.version="+specVersion)
	Expect(err).NotTo(HaveOccurred())
	By("starting the binary once, so its slow first start lands outside every spec")
	first, err := gexec.Start(exec.Command(bin, "--version"), GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(first, time.Minute).Should(gexec.Exit(0))

	state := suiteState{Binary: bin}
	if dbSpecsSelected() {
		started, err := testsupport.Start(ctx, domain.Migrate)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(started.Stop)
		state.AdminURL = started.URL()
	}
	raw, err := json.Marshal(state)
	Expect(err).NotTo(HaveOccurred())
	return raw
}, func(ctx context.Context, raw []byte) {
	var state suiteState
	Expect(json.Unmarshal(raw, &state)).To(Succeed())
	binary = state.Binary
	if state.AdminURL == "" {
		return
	}
	var err error
	pg, err = testsupport.Attach(ctx, state.AdminURL)
	Expect(err).NotTo(HaveOccurred())
})

// The first process built the binary, so it removes it, once every
// process is done with it.
var _ = SynchronizedAfterSuite(func() {}, func() {
	gexec.CleanupBuildArtifacts()
})
