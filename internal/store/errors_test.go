package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// The errors the domain distinguishes, provoked from PostgreSQL itself so
// a predicate cannot pass against an error the server never sends.
var _ = Describe("Errors the store recognises", Label(testsupport.DBLabel), func() {
	var s *store.Store

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
	})

	// predicates is each conflict predicate by name, so a spec can assert
	// that the one it provoked matches and every other one does not.
	predicates := map[string]func(error) bool{
		"idempotency key": store.IsIdempotencyKeyConflict,
		"live lease":      store.IsLiveLeaseConflict,
		"identity":        store.IsIdentityConflict,
	}
	expectOnly := func(err error, name string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		for other, matches := range predicates {
			Expect(matches(err)).To(Equal(other == name), "the %s predicate on %v", other, err)
		}
	}

	// fixture is an organization with a project, a node and a principal,
	// committed so a later transaction can collide with it.
	type fixture struct{ org, node, principal string }
	given := func(ctx context.Context) fixture {
		GinkgoHelper()
		var f fixture
		Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			org, err := tx.CreateOrganization(ctx, store.CreateOrganizationParams{ID: newULID(), Name: "acme"})
			Expect(err).NotTo(HaveOccurred())
			p, err := tx.CreateProject(ctx, store.CreateProjectParams{ID: newULID(), OrgID: org.ID, Slug: "platform", Title: "platform", Priority: "p2"})
			Expect(err).NotTo(HaveOccurred())
			n, err := tx.CreateNode(ctx, store.CreateNodeParams{ID: newULID(), Number: 1, OrgID: org.ID, ProjectID: p.ID, Kind: "task", Title: "one", Priority: "p2", Labels: []string{}})
			Expect(err).NotTo(HaveOccurred())
			pr, err := tx.CreatePrincipal(ctx, store.CreatePrincipalParams{ID: newULID(), OrgID: org.ID, Kind: "agent"})
			Expect(err).NotTo(HaveOccurred())
			f = fixture{org: org.ID, node: n.ID, principal: pr.ID}
			return nil
		})).To(Succeed())
		return f
	}

	It("The store recognises a missing row", func(ctx SpecContext) {
		By("given a transaction")
		err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			By("when a generated query that returns one row finds none")
			_, err := tx.GetNode(ctx, store.GetNodeParams{OrgID: newULID(), ID: newULID()})
			return fmt.Errorf("read node: %w", err)
		})

		By("then the store's not-found predicate matches the error")
		Expect(errors.Is(err, store.ErrNoRows)).To(BeTrue())
		By("and no conflict predicate does")
		expectOnly(err, "")
	})

	It("recognises a duplicate idempotency key, and only that", func(ctx SpecContext) {
		f := given(ctx)
		key := store.CreateIdempotencyKeyParams{OrgID: f.org, RequestID: "req-1", RequestHash: "sha256:" + strings.Repeat("0", 64), Response: []byte("{}")}
		Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := tx.CreateIdempotencyKey(ctx, key)
			return err
		})).To(Succeed())

		err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := tx.CreateIdempotencyKey(ctx, key)
			return err
		})
		expectOnly(err, "idempotency key")
		Expect(errors.Is(err, store.ErrNoRows)).To(BeFalse())
	})

	It("recognises a second live lease on a node, and only that", func(ctx SpecContext) {
		f := given(ctx)
		lease := func() store.CreateLeaseParams {
			return store.CreateLeaseParams{ID: newULID(), OrgID: f.org, NodeID: f.node, Holder: f.principal, Harness: "spec", AgentRef: "spec", ExpiresAt: time.Now().Add(time.Hour)}
		}
		Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := tx.CreateLease(ctx, lease())
			return err
		})).To(Succeed())

		err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := tx.CreateLease(ctx, lease())
			return err
		})
		expectOnly(err, "live lease")
	})

	It("recognises an identity another principal holds, and only that", func(ctx SpecContext) {
		f := given(ctx)
		identity := store.AddPrincipalIdentityParams{OrgID: f.org, ID: f.principal, Scheme: "unix", Value: "1000"}
		Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := tx.AddPrincipalIdentity(ctx, identity)
			return err
		})).To(Succeed())

		err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := tx.AddPrincipalIdentity(ctx, identity)
			return err
		})
		expectOnly(err, "identity")
	})
})
