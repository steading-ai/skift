package domain_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// claimReq is a claim of the spec's project with the given request id and
// TTL, from a harness the specs do not care about.
func claimReq(requestID string, ttl time.Duration) domain.ClaimRequest {
	return domain.ClaimRequest{RequestID: requestID, Project: "platform", TTL: ttl, Harness: "spec", AgentRef: "spec"}
}

// claim runs one Claim and records the node it returns under its
// reference.
func (g *graph) claim(ctx context.Context, actor domain.Actor, req domain.ClaimRequest) (*domain.Claimed, error) {
	GinkgoHelper()
	c, err := domain.Claim(ctx, g.s, pol, g.org, actor, req)
	if c != nil {
		g.ids[fmt.Sprintf("platform#%d", c.Node.Number)] = c.Node.ID
	}
	return c, err
}

// mustClaim is claim for fixtures: it must return a node.
func (g *graph) mustClaim(ctx context.Context, actor domain.Actor, req domain.ClaimRequest) *domain.Claimed {
	GinkgoHelper()
	c, err := g.claim(ctx, actor, req)
	Expect(err).NotTo(HaveOccurred())
	Expect(c).NotTo(BeNil(), "expected a claim, got nothing")
	return c
}

// age rewrites a lease's clock so that it was claimed `since` ago and has
// `remaining` left, or has expired when remaining is negative.
func (g *graph) age(ctx context.Context, leaseID string, since, remaining time.Duration) {
	GinkgoHelper()
	now := time.Now()
	_, err := g.s.Pool().Exec(ctx, `UPDATE leases SET claimed_at = $2, expires_at = $3 WHERE org_id = $1 AND id = $4`,
		g.org, now.Add(-since), now.Add(remaining), leaseID)
	Expect(err).NotTo(HaveOccurred())
}

func (g *graph) getLease(ctx context.Context, id string) store.Lease {
	GinkgoHelper()
	l, err := g.s.Queries().GetLease(ctx, store.GetLeaseParams{OrgID: g.org, ID: id})
	Expect(err).NotTo(HaveOccurred())
	return l
}

// liveLeases counts the unreleased, unexpired leases in the organization.
func (g *graph) liveLeases(ctx context.Context) int {
	GinkgoHelper()
	var n int
	Expect(g.s.Pool().QueryRow(ctx, `SELECT count(*) FROM leases WHERE org_id = $1 AND released_at IS NULL AND expires_at > now()`, g.org).Scan(&n)).To(Succeed())
	return n
}

// nodeEvents is the history of one node, in order.
func (g *graph) nodeEvents(ctx context.Context, ref string) []store.Event {
	GinkgoHelper()
	id := g.id(ref)
	events, err := g.s.Queries().ListEventsForNode(ctx, store.ListEventsForNodeParams{OrgID: g.org, NodeID: &id, PageSize: 100})
	Expect(err).NotTo(HaveOccurred())
	return events
}

// update is an update_node operation retitling a node.
func update(node, title string) domain.Op {
	return domain.Op{UpdateNode: &domain.UpdateNode{Node: node, Title: &title}}
}

// applyWithLease is apply for a patch that carries a lease id.
func (g *graph) applyWithLease(ctx context.Context, actor domain.Actor, requestID, leaseID string, ops ...domain.Op) (*domain.Result, error) {
	GinkgoHelper()
	return g.applyPatch(ctx, actor, domain.Patch{RequestID: requestID, Project: "platform", LeaseID: leaseID, Ops: ops})
}

var _ = Describe("Leases", Label(testsupport.DBLabel), func() {
	var (
		s     *store.Store
		g     *graph
		actor domain.Actor
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		actor = g.principal(ctx)
	})

	Describe("Scenarios", func() {
		It("A stale worker cannot complete reclaimed work", func(ctx SpecContext) {
			By("given agent A holds a lease on node 7")
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 7, nil) })
			agentA, agentB := g.agent(ctx), g.agent(ctx)
			first := g.mustClaim(ctx, agentA, claimReq("a-claim", time.Minute))
			Expect(first.Node.Number).To(Equal(int64(7)))

			By("and that lease expires and agent B claims node 7")
			g.age(ctx, first.Lease.ID, 2*time.Minute, -time.Minute)
			second := g.mustClaim(ctx, agentB, claimReq("b-claim", time.Minute))
			Expect(second.Node.ID).To(Equal(first.Node.ID))
			Expect(ops(second.Events)).To(Equal([]string{domain.OpExpireLease, domain.OpClaimNode}))

			By("when agent A completes node 7 with its old lease id")
			_, err := domain.Release(ctx, s, pol, g.org, agentA, "a-complete", first.Lease.ID, domain.ReleaseComplete, "")

			By("then the write is rejected")
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))
			_, err = g.applyWithLease(ctx, agentA, "a-patch", first.Lease.ID, update("#7", "Done by A"))
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))

			By("and node 7 remains open and held by agent B")
			Expect(g.getNode(ctx, "#7").Status).To(Equal("open"))
			Expect(g.getNode(ctx, "#7").Title).To(Equal("Node 7"))
			e := g.explain(ctx, "#7")
			Expect(e.Reasons).To(HaveExactElements(BeReason(domain.ReasonLeased)))
			Expect(e.Reasons[0].Lease.ID).To(Equal(second.Lease.ID))
			Expect(e.Reasons[0].Lease.Holder).To(Equal(agentB.PrincipalID))
			Expect(*g.getLease(ctx, first.Lease.ID).ReleaseReason).To(Equal(domain.ReleaseExpired))
		})

		It("Exactly one claimant wins", func(ctx SpecContext) {
			By("given one ready node")
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })

			By("when 100 concurrent ClaimNext calls run")
			const claimants = 100
			results := make(chan *domain.Claimed, claimants)
			var wg sync.WaitGroup
			for i := range claimants {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					c, err := domain.ClaimNext(ctx, s, pol, g.org, actor, claimReq(fmt.Sprintf("claim-%d", i), 0))
					Expect(err).NotTo(HaveOccurred())
					results <- c
				}()
			}
			wg.Wait()
			close(results)

			By("then exactly one returns the node")
			var won []*domain.Claimed
			for c := range results {
				if c != nil {
					won = append(won, c)
				}
			}
			Expect(won).To(HaveLen(1))
			Expect(won[0].Node.Number).To(Equal(int64(1)))

			By("and exactly one live lease exists")
			Expect(g.liveLeases(ctx)).To(Equal(1))
			var leases int
			Expect(s.Pool().QueryRow(ctx, `SELECT count(*) FROM leases WHERE org_id = $1`, g.org).Scan(&leases)).To(Succeed())
			Expect(leases).To(Equal(1))
		})

		It("A lost claim response does not consume two nodes", func(ctx SpecContext) {
			By("given two ready nodes")
			g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))

			By("when a claim commits but its response is lost and the client retries with the same idempotency key")
			first := g.mustClaim(ctx, actor, claimReq("req-1", 0))
			Expect(first.Replayed).To(BeFalse())
			again := g.mustClaim(ctx, actor, claimReq("req-1", 0))

			By("then the same node and lease are returned")
			Expect(again.Replayed).To(BeTrue())
			again.Replayed = false
			Expect(again).To(Equal(first))
			Expect(again.Node.Number).To(Equal(int64(1)))

			By("and the second node is still ready")
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{2}))
			Expect(g.liveLeases(ctx)).To(Equal(1))
		})

		It("Work in progress keeps its lease", func(ctx SpecContext) {
			By("given a lease with a 60 second TTL")
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			c := g.mustClaim(ctx, actor, claimReq("claim", 60*time.Second))
			Expect(c.Lease.ExpiresAt).To(BeTemporally("~", time.Now().Add(60*time.Second), 5*time.Second))

			By("when the holder applies a patch to its node at 50 seconds")
			g.age(ctx, c.Lease.ID, 50*time.Second, 10*time.Second)
			before := g.getLease(ctx, c.Lease.ID)
			_, err := g.applyWithLease(ctx, actor, "work", c.Lease.ID, update("#1", "Progress"))
			Expect(err).NotTo(HaveOccurred())

			By("then the lease expiry moves forward")
			after := g.getLease(ctx, c.Lease.ID)
			Expect(after.ExpiresAt).To(BeTemporally(">", before.ExpiresAt))
			Expect(after.ExpiresAt).To(BeTemporally("~", time.Now().Add(domain.DefaultLeaseTTL), 5*time.Second))
			Expect(after.ReleasedAt).To(BeNil())
		})

		It("Waiting does not exhaust the connection pool", func(ctx SpecContext) {
			By("given a pool of 10 connections")
			config := s.Pool().Config()
			config.MaxConns = 10
			pool, err := pgxpool.NewWithConfig(ctx, config)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)
			small := store.FromPool(pool)

			By("when 50 clients call ClaimNext with a 30 second wait and no work exists")
			waitCtx, stop := context.WithCancel(ctx)
			defer stop()
			var wg sync.WaitGroup
			for i := range 50 {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					req := claimReq(fmt.Sprintf("wait-%d", i), 0)
					req.Wait = 30 * time.Second
					c, err := domain.ClaimNext(waitCtx, small, pol, g.org, actor, req)
					Expect(c).To(BeNil())
					Expect(err).To(MatchError(context.Canceled))
				}()
			}

			By("then other queries continue to succeed")
			for range 20 {
				queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				var one int
				Expect(pool.QueryRow(queryCtx, `SELECT 1`).Scan(&one)).To(Succeed())
				cancel()
				Expect(one).To(Equal(1))
			}

			By("and the waiters hold no connection between polls")
			Eventually(func() int32 { return pool.Stat().AcquiredConns() }).Should(BeZero())
			var open int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND state = 'idle in transaction'`).Scan(&open)).To(Succeed())
			Expect(open).To(BeZero())

			stop()
			wg.Wait()
		})
	})

	Describe("Fencing", func() {
		var held *domain.Claimed

		BeforeEach(func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))
			held = g.mustClaim(ctx, actor, claimReq("hold", 0))
			Expect(held.Node.Number).To(Equal(int64(1)))
		})

		It("rejects a write to a held node carrying no lease id", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "bare", update("#1", "Bare"))
			Expect(err).To(MatchError(domain.ErrLeaseRequired))
			Expect(g.getNode(ctx, "#1").Title).To(Equal("A"))
		})

		It("rejects a write carrying a stale lease id", func(ctx SpecContext) {
			_, err := domain.Release(ctx, s, pol, g.org, actor, "release", held.Lease.ID, domain.ReleaseCrash, "")
			Expect(err).NotTo(HaveOccurred())
			_, err = g.applyWithLease(ctx, actor, "stale", held.Lease.ID, update("#1", "Stale"))
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))
		})

		It("rejects a write carrying an expired lease, even before the timer has released it", func(ctx SpecContext) {
			g.age(ctx, held.Lease.ID, time.Minute, -time.Second)
			_, err := g.applyWithLease(ctx, actor, "expired", held.Lease.ID, update("#1", "Late"))
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))
			Expect(g.getLease(ctx, held.Lease.ID).ReleasedAt).To(BeNil())
		})

		It("rejects a write to a node held by a different lease", func(ctx SpecContext) {
			other := g.principal(ctx)
			theirs := g.mustClaim(ctx, other, claimReq("theirs", 0))
			Expect(theirs.Node.Number).To(Equal(int64(2)))

			_, err := g.applyWithLease(ctx, actor, "cross", held.Lease.ID, update("#2", "Not mine"))
			Expect(err).To(MatchError(domain.ErrLeaseMismatch))
		})

		It("rejects a write carrying another principal's lease", func(ctx SpecContext) {
			other := g.principal(ctx)
			_, err := g.applyWithLease(ctx, other, "borrowed", held.Lease.ID, update("#1", "Borrowed"))
			Expect(err).To(MatchError(domain.ErrNotLeaseHolder))
		})

		It("fences every write that changes a held node's readiness, not only its fields", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "edge", addEdge("#2", "#1"))
			Expect(err).To(MatchError(domain.ErrLeaseRequired))
			_, err = g.apply(ctx, actor, "child", domain.Op{CreateNode: &domain.CreateNode{Title: "Child", Parent: "#1"}})
			Expect(err).To(MatchError(domain.ErrLeaseRequired))
			_, err = g.apply(ctx, actor, "drop", drop("#1", domain.DropSuperseded))
			Expect(err).To(MatchError(domain.ErrLeaseRequired))

			By("while the holder may do all of them")
			_, err = g.applyWithLease(ctx, actor, "held", held.Lease.ID,
				addEdge("#2", "#1"),
				domain.Op{CreateNode: &domain.CreateNode{Title: "Child", Parent: "#1"}},
			)
			Expect(err).NotTo(HaveOccurred())
		})

		It("leaves writes to unheld nodes alone, with or without a lease id", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "free", update("#2", "Free"))
			Expect(err).NotTo(HaveOccurred())
			_, err = g.applyWithLease(ctx, actor, "free-with-lease", held.Lease.ID, update("#2", "Freer"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("checks a live lease is the actor's before doing any work", func(ctx SpecContext) {
			_, err := g.applyWithLease(ctx, actor, "unknown", domain.NewULID(), create("a", "A"))
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))
			Expect(g.countEvents(ctx, "unknown")).To(BeZero())
		})
	})

	Describe("Claim", func() {
		It("returns nothing when no node is ready, without consuming the request id", func(ctx SpecContext) {
			c, err := g.claim(ctx, actor, claimReq("empty", 0))
			Expect(err).NotTo(HaveOccurred())
			Expect(c).To(BeNil())

			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			c = g.mustClaim(ctx, actor, claimReq("empty", 0))
			Expect(c.Replayed).To(BeFalse())
			Expect(c.Node.Number).To(Equal(int64(1)))
		})

		It("clamps a TTL above the ceiling rather than rejecting it", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			c := g.mustClaim(ctx, actor, claimReq("long", 3*domain.MaxLeaseTTL))
			Expect(c.Lease.ExpiresAt).To(BeTemporally("~", time.Now().Add(domain.MaxLeaseTTL), 5*time.Second))
		})

		It("applies the default TTL when none is requested", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			c := g.mustClaim(ctx, actor, claimReq("default", 0))
			Expect(c.Lease.ExpiresAt).To(BeTemporally("~", time.Now().Add(domain.DefaultLeaseTTL), 5*time.Second))
		})

		It("records the claim as an event carrying the lease and the request id", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			c := g.mustClaim(ctx, actor, claimReq("claim", 0))

			events := g.nodeEvents(ctx, "#1")
			Expect(ops(events)).To(Equal([]string{domain.OpClaimNode}))
			Expect(*events[0].RequestID).To(Equal("claim"))
			Expect(events[0].PrincipalID).To(Equal(actor.PrincipalID))
			Expect(field(events[0].After, "ID")).To(Equal(c.Lease.ID))
			Expect(c.Events).To(Equal(events))
		})

		It("takes the most urgent node the filters admit, and skips one another claimant is taking", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeWith(ctx, tx, 1, nodeOpts{priority: "p3"})
				g.nodeWith(ctx, tx, 2, nodeOpts{priority: "p1"})
				g.nodeWith(ctx, tx, 3, nodeOpts{priority: "p0", kind: "spike"})
			})

			By("a claim for tasks only skips the p0 spike")
			req := claimReq("tasks", 0)
			req.Kind = "task"
			Expect(g.mustClaim(ctx, actor, req).Node.Number).To(Equal(int64(2)))

			By("a node whose row another transaction holds is skipped, not waited for")
			conn, err := s.Pool().Acquire(ctx)
			Expect(err).NotTo(HaveOccurred())
			tx, err := conn.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			_, err = tx.Exec(ctx, `SELECT 1 FROM nodes WHERE org_id = $1 AND id = $2 FOR UPDATE`, g.org, g.id("#3"))
			Expect(err).NotTo(HaveOccurred())
			Expect(g.mustClaim(ctx, actor, claimReq("any", 0)).Node.Number).To(Equal(int64(1)))
			Expect(tx.Rollback(ctx)).To(Succeed())
			conn.Release()

			Expect(g.mustClaim(ctx, actor, claimReq("last", 0)).Node.Number).To(Equal(int64(3)))
			Expect(g.liveLeases(ctx)).To(Equal(3))
		})

		It("rejects the same request id with different content", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			g.mustClaim(ctx, actor, claimReq("req", time.Minute))
			_, err := g.claim(ctx, actor, claimReq("req", time.Hour))
			var conflict *domain.RequestConflictError
			Expect(errors.As(err, &conflict)).To(BeTrue(), "expected a request conflict, got %v", err)
		})

		It("does not count the wait as content, so a retry may wait differently", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			first := g.mustClaim(ctx, actor, claimReq("req", 0))
			req := claimReq("req", 0)
			req.Wait = 10 * time.Second
			again, err := domain.ClaimNext(ctx, s, pol, g.org, actor, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.Replayed).To(BeTrue())
			Expect(again.Lease.ID).To(Equal(first.Lease.ID))
		})

		It("refuses an archived project, a negative TTL, and a missing request id or actor", func(ctx SpecContext) {
			_, err := g.claim(ctx, actor, claimReq("", 0))
			Expect(err).To(MatchError(domain.ErrNoRequestID))
			_, err = g.claim(ctx, domain.Actor{}, claimReq("no-actor", 0))
			Expect(err).To(MatchError(domain.ErrNoActor))
			_, err = g.claim(ctx, actor, claimReq("negative", -time.Second))
			Expect(err).To(MatchError(domain.ErrNegativeDuration))

			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.archive(ctx, tx, g.project) })
			_, err = g.claim(ctx, actor, claimReq("archived", 0))
			Expect(err).To(MatchError(domain.ErrProjectArchived))
		})
	})

	Describe("ClaimNext", func() {
		It("returns work that appears during the wait", func(ctx SpecContext) {
			req := claimReq("wait", 0)
			req.Wait = 10 * time.Second
			done := make(chan *domain.Claimed, 1)
			go func() {
				defer GinkgoRecover()
				c, err := domain.ClaimNext(ctx, s, pol, g.org, actor, req)
				Expect(err).NotTo(HaveOccurred())
				done <- c
			}()
			Consistently(done, 300*time.Millisecond).ShouldNot(Receive())

			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })

			var c *domain.Claimed
			Eventually(done, 5*time.Second).Should(Receive(&c))
			Expect(c).NotTo(BeNil())
			Expect(c.Node.Number).To(Equal(int64(1)))
		})

		It("returns empty once the wait elapses", func(ctx SpecContext) {
			req := claimReq("wait", 0)
			req.Wait = 300 * time.Millisecond
			started := time.Now()
			c, err := domain.ClaimNext(ctx, s, pol, g.org, actor, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(c).To(BeNil())
			Expect(time.Since(started)).To(BeNumerically(">=", 300*time.Millisecond))
			Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second))
		})

		It("stops when its context is cancelled", func(ctx SpecContext) {
			req := claimReq("wait", 0)
			req.Wait = 30 * time.Second
			waitCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				_, err := domain.ClaimNext(waitCtx, s, pol, g.org, actor, req)
				done <- err
			}()
			cancel()
			Eventually(done, 5*time.Second).Should(Receive(MatchError(context.Canceled)))
		})
	})

	Describe("Heartbeat", func() {
		var c *domain.Claimed

		BeforeEach(func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			c = g.mustClaim(ctx, actor, claimReq("claim", time.Minute))
		})

		It("renews the lease to the requested TTL from now, clamped, and records it", func(ctx SpecContext) {
			change, err := domain.Heartbeat(ctx, s, pol, g.org, actor, "beat", c.Lease.ID, 2*domain.MaxLeaseTTL)
			Expect(err).NotTo(HaveOccurred())
			Expect(change.Lease.ExpiresAt).To(BeTemporally("~", time.Now().Add(domain.MaxLeaseTTL), 5*time.Second))
			Expect(g.getLease(ctx, c.Lease.ID).ExpiresAt).To(BeTemporally("==", change.Lease.ExpiresAt))
			Expect(ops(g.nodeEvents(ctx, "#1"))).To(Equal([]string{domain.OpClaimNode, domain.OpHeartbeatLease}))

			By("and a heartbeat with no TTL takes the default")
			change, err = domain.Heartbeat(ctx, s, pol, g.org, actor, "beat-2", c.Lease.ID, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(change.Lease.ExpiresAt).To(BeTemporally("~", time.Now().Add(domain.DefaultLeaseTTL), 5*time.Second))
		})

		It("is fenced: another principal, a released lease, and an expired lease are rejected", func(ctx SpecContext) {
			_, err := domain.Heartbeat(ctx, s, pol, g.org, g.principal(ctx), "beat", c.Lease.ID, 0)
			Expect(err).To(MatchError(domain.ErrNotLeaseHolder))

			g.age(ctx, c.Lease.ID, time.Minute, -time.Second)
			_, err = domain.Heartbeat(ctx, s, pol, g.org, actor, "beat", c.Lease.ID, 0)
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))
			Expect(g.getLease(ctx, c.Lease.ID).ExpiresAt).To(BeTemporally("<", time.Now()))

			g.age(ctx, c.Lease.ID, time.Minute, time.Minute)
			_, err = domain.Release(ctx, s, pol, g.org, actor, "release", c.Lease.ID, domain.ReleaseGate, "")
			Expect(err).NotTo(HaveOccurred())
			_, err = domain.Heartbeat(ctx, s, pol, g.org, actor, "beat", c.Lease.ID, 0)
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))
		})

		It("replays under its request id and rejects a negative TTL or no lease id", func(ctx SpecContext) {
			first, err := domain.Heartbeat(ctx, s, pol, g.org, actor, "beat", c.Lease.ID, time.Minute)
			Expect(err).NotTo(HaveOccurred())
			again, err := domain.Heartbeat(ctx, s, pol, g.org, actor, "beat", c.Lease.ID, time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.Replayed).To(BeTrue())
			again.Replayed = false
			Expect(again).To(Equal(first))
			Expect(g.countEvents(ctx, "beat")).To(Equal(1))

			_, err = domain.Heartbeat(ctx, s, pol, g.org, actor, "neg", c.Lease.ID, -time.Second)
			Expect(err).To(MatchError(domain.ErrNegativeDuration))
			_, err = domain.Heartbeat(ctx, s, pol, g.org, actor, "none", "", 0)
			Expect(err).To(MatchError(domain.ErrNoLeaseID))
		})
	})

	Describe("Release", func() {
		var c *domain.Claimed

		BeforeEach(func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			c = g.mustClaim(ctx, actor, claimReq("claim", 0))
			Expect(g.ready(ctx, domain.ReadyQuery{})).To(BeEmpty())
		})

		It("ends the lease with a reason, records it, and returns the node to the ready set", func(ctx SpecContext) {
			change, err := domain.Release(ctx, s, pol, g.org, actor, "release", c.Lease.ID, domain.ReleaseCrash, "out of memory")
			Expect(err).NotTo(HaveOccurred())
			Expect(change.Lease.ReleasedAt).NotTo(BeNil())
			Expect(*change.Lease.ReleaseReason).To(Equal(domain.ReleaseCrash))

			events := g.nodeEvents(ctx, "#1")
			Expect(ops(events)).To(Equal([]string{domain.OpClaimNode, domain.OpReleaseLease}))
			Expect(*events[1].Reason).To(Equal("crash: out of memory"))
			Expect(field(events[1].Before, "ReleasedAt")).To(BeNil())
			Expect(field(events[1].After, "ReleaseReason")).To(Equal(domain.ReleaseCrash))

			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))
			Expect(g.liveLeases(ctx)).To(BeZero())
		})

		It("is fenced: a released or expired lease, or another principal's, is rejected", func(ctx SpecContext) {
			_, err := domain.Release(ctx, s, pol, g.org, g.principal(ctx), "theirs", c.Lease.ID, domain.ReleaseCrash, "")
			Expect(err).To(MatchError(domain.ErrNotLeaseHolder))

			_, err = domain.Release(ctx, s, pol, g.org, actor, "once", c.Lease.ID, domain.ReleaseBudget, "")
			Expect(err).NotTo(HaveOccurred())
			_, err = domain.Release(ctx, s, pol, g.org, actor, "twice", c.Lease.ID, domain.ReleaseCrash, "")
			Expect(err).To(MatchError(domain.ErrLeaseNotLive))
			Expect(*g.getLease(ctx, c.Lease.ID).ReleaseReason).To(Equal(domain.ReleaseBudget))
		})

		It("requires a client reason, and leaves expired to the server", func(ctx SpecContext) {
			_, err := domain.Release(ctx, s, pol, g.org, actor, "r", c.Lease.ID, "", "")
			Expect(err).To(MatchError(domain.ErrReasonRequired))
			_, err = domain.Release(ctx, s, pol, g.org, actor, "r", c.Lease.ID, domain.ReleaseExpired, "")
			Expect(err).To(MatchError(domain.ErrReasonRequired))
			_, err = domain.Release(ctx, s, pol, g.org, actor, "r", c.Lease.ID, "bored", "")
			Expect(err).To(MatchError(`unknown release reason "bored"`))
			Expect(g.liveLeases(ctx)).To(Equal(1))
		})
	})

	Describe("Expiry", func() {
		It("returns the node to the ready set and writes an event", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))
			expired := g.mustClaim(ctx, actor, claimReq("one", 0))
			live := g.mustClaim(ctx, actor, claimReq("two", 0))
			Expect(g.ready(ctx, domain.ReadyQuery{})).To(BeEmpty())
			g.age(ctx, expired.Lease.ID, time.Hour, -time.Minute)

			By("when the timer runs as the service principal")
			reaper := g.principalOf(ctx, "service")
			released, err := domain.ExpireLeases(ctx, s, pol, g.org, reaper, 100)
			Expect(err).NotTo(HaveOccurred())

			By("then only the expired lease is released, as expired")
			Expect(released).To(HaveLen(1))
			Expect(released[0].ID).To(Equal(expired.Lease.ID))
			Expect(*released[0].ReleaseReason).To(Equal(domain.ReleaseExpired))
			Expect(g.getLease(ctx, live.Lease.ID).ReleasedAt).To(BeNil())
			Expect(ops(g.nodeEvents(ctx, "#2"))).To(Equal([]string{domain.OpCreateNode, domain.OpClaimNode}))

			By("and its node is ready again")
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))

			By("and the event names the reaper and no request")
			events := g.nodeEvents(ctx, "#1")
			Expect(ops(events)).To(Equal([]string{domain.OpCreateNode, domain.OpClaimNode, domain.OpExpireLease}))
			expiry := events[2]
			Expect(expiry.PrincipalID).To(Equal(reaper.PrincipalID))
			Expect(expiry.PrincipalKind).To(Equal("service"))
			Expect(expiry.RequestID).To(BeNil())
			Expect(*expiry.Reason).To(Equal(domain.ReleaseExpired))
			Expect(field(expiry.After, "ReleaseReason")).To(Equal(domain.ReleaseExpired))

			By("and a second run finds nothing")
			released, err = domain.ExpireLeases(ctx, s, pol, g.org, reaper, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(released).To(BeEmpty())
		})

		It("is done by a claim before the timer gets round to it", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			first := g.mustClaim(ctx, actor, claimReq("one", 0))
			g.age(ctx, first.Lease.ID, time.Hour, -time.Minute)

			second := g.mustClaim(ctx, actor, claimReq("two", 0))
			Expect(second.Node.ID).To(Equal(first.Node.ID))
			Expect(*g.getLease(ctx, first.Lease.ID).ReleaseReason).To(Equal(domain.ReleaseExpired))
			Expect(ops(g.nodeEvents(ctx, "#1"))).To(Equal([]string{domain.OpClaimNode, domain.OpExpireLease, domain.OpClaimNode}))
			Expect(*g.nodeEvents(ctx, "#1")[1].RequestID).To(Equal("two"))
			Expect(g.liveLeases(ctx)).To(Equal(1))
		})
	})
})
