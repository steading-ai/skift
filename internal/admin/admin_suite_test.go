package admin_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/testsupport"
)

func TestAdmin(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "admin")
}

// pg is the server every spec in this suite clones from. Its template
// carries the full schema.
var pg *testsupport.Postgres

// pol is the default policy bundle, for specs that check a bootstrapped
// principal through the domain the way the service would.
var pol *policy.Bundle

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	started, err := testsupport.Start(ctx, admin.Migrate)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(started.Stop)
	return []byte(started.URL())
}, func(ctx context.Context, adminURL []byte) {
	var err error
	pol, err = policy.LoadDefault(ctx)
	Expect(err).NotTo(HaveOccurred())
	pg, err = testsupport.Attach(ctx, string(adminURL))
	Expect(err).NotTo(HaveOccurred())
})
