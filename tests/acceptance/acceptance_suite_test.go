// Package acceptance_test is the end-to-end suite: the product requirements
// as scenarios, run against the built dg binary serving a real PostgreSQL,
// and driven only through the interfaces a user or an agent has, the CLI
// and the MCP server. Nothing here reads or writes the database directly
// once a scenario starts; see harness_test.go for the one bootstrap the
// service has no public interface for.
package acceptance_test

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/testsupport"
)

func TestAcceptance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "acceptance")
}

// pg is the server every scenario clones its database from. The template
// carries the schema and the principals of one organization.
var pg *testsupport.Postgres

// binary is the dg built for this run: the service, the CLI, and the MCP
// server are all faces of it.
var binary string

// pol is the default policy bundle, loaded once per process for the
// in-process tailnet server; dg serve loads its own.
var pol *policy.Bundle

// suiteState is what the first process hands the others.
type suiteState struct {
	AdminURL string `json:"admin_url"`
	Binary   string `json:"binary"`
}

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	started, err := testsupport.Start(ctx, migrateAndSeed)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(started.Stop)

	bin, err := gexec.Build("github.com/ivy/dagger/cmd/dg")
	Expect(err).NotTo(HaveOccurred())

	raw, err := json.Marshal(suiteState{AdminURL: started.URL(), Binary: bin})
	Expect(err).NotTo(HaveOccurred())
	return raw
}, func(ctx context.Context, raw []byte) {
	var state suiteState
	Expect(json.Unmarshal(raw, &state)).To(Succeed())
	binary = state.Binary

	var err error
	pol, err = policy.LoadDefault(ctx)
	Expect(err).NotTo(HaveOccurred())
	pg, err = testsupport.Attach(ctx, state.AdminURL)
	Expect(err).NotTo(HaveOccurred())
})

var _ = SynchronizedAfterSuite(func() {}, func() {
	gexec.CleanupBuildArtifacts()
})
