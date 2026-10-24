package domain_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// The product invariants under concurrency and failure. Each spec here
// corresponds to a failure that is silent if unhandled: a cycle two
// patches commit between them, a node two claimants both hold, a stale
// worker landing a late write, a claim and a blocker crossing, a retry
// doing the work twice, a crash leaving half a transaction, a tenant
// reading another's rows. See the Concurrency and Fencing sections of
// docs/domain-model.md and docs/adrs/006-provenance-not-authority.md.

// tracedStore opens a second pool on the spec's database that reports
// every statement to tracer.
func tracedStore(ctx context.Context, pool *pgxpool.Pool, tracer pgx.QueryTracer) *store.Store {
	GinkgoHelper()
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(traced.Close)
	return store.FromPool(traced)
}

// backendKiller is a query tracer that terminates the server backend of a
// connection just before the connection sends its at-th statement, which
// is what the server sees when the service dies mid-transaction: the
// session ends and everything uncommitted is rolled back. Statements are
// counted from 1 across the pool it traces; armed at zero it kills
// nothing and only counts.
type backendKiller struct {
	// admin is an untraced pool the termination is issued through.
	admin  *pgxpool.Pool
	at     atomic.Int64
	n      atomic.Int64
	killed atomic.Bool
}

// arm resets the count and sets the statement to kill before.
func (k *backendKiller) arm(at int64) {
	k.at.Store(at)
	k.n.Store(0)
	k.killed.Store(false)
}

func (k *backendKiller) TraceQueryStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if k.n.Add(1) != k.at.Load() {
		return ctx
	}
	pid := int32(conn.PgConn().PID())
	_, err := k.admin.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid)
	Expect(err).NotTo(HaveOccurred())
	// pg_terminate_backend only signals. Wait for the backend to be gone,
	// so that its transaction has been rolled back by the time the spec
	// looks at the state.
	Eventually(func(g Gomega) {
		var alive bool
		g.Expect(k.admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive)).To(Succeed())
		g.Expect(alive).To(BeFalse())
	}).Should(Succeed())
	k.killed.Store(true)
	return ctx
}

func (k *backendKiller) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// pauser is a query tracer that holds the connection that has just run
// the first statement containing match until the spec releases it, so an
// operation can be stopped at a chosen point while another runs against
// it. Only the first match pauses; a retry after release runs freely.
type pauser struct {
	match string
	// reached is closed when the matching statement has completed.
	reached chan struct{}
	// release is closed by the spec to let the operation go on.
	release chan struct{}
	once    sync.Once
}

func newPauser(match string) *pauser {
	return &pauser{match: match, reached: make(chan struct{}), release: make(chan struct{})}
}

type pauseKey struct{}

func (p *pauser) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, p.match) {
		return context.WithValue(ctx, pauseKey{}, true)
	}
	return ctx
}

func (p *pauser) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(pauseKey{}) == nil {
		return
	}
	p.once.Do(func() { close(p.reached) })
	<-p.release
}

// tornCase is one operation the restart scenario kills before every
// statement it sends. arm lays fresh fixtures and returns the operation,
// bound to whichever store it is run through, and an observation of the
// state the operation changes. The observation is before until the
// operation commits and after once it has; anything else is torn.
type tornCase struct {
	name          string
	arm           func() (run func(*store.Store) error, observe func() any)
	before, after any
}

// leaseState is what a claim leaves behind: the leases on the node, the
// live ones, the events under its request id, and the key for a retry.
type leaseState struct {
	Leases, Live, Events int
	Key                  bool
}

// releaseState is what a release leaves behind: the lease released with
// its reason, the event, and the key.
type releaseState struct {
	Released bool
	Reason   string
	Events   int
	Key      bool
}

// patchState is what a patch retitling one node and blocking it leaves
// behind.
type patchState struct {
	Retitled, Edge bool
	Events         int
	Key            bool
}

// reapState is what expiring a pair of leases leaves behind.
type reapState struct {
	Released, Events int
}

// labelled inserts node number with a label of its own, so a claim can be
// filtered to exactly that node whatever else is ready.
func (g *graph) labelled(ctx context.Context, number int64) (ref, label string) {
	GinkgoHelper()
	ref, label = fmt.Sprintf("#%d", number), fmt.Sprintf("round-%d", number)
	g.given(ctx, func(ctx context.Context, tx store.Tx) {
		g.nodeWith(ctx, tx, number, nodeOpts{labels: []string{label}})
	})
	return ref, label
}

// hasKey reports whether the organization holds an idempotency key for
// the request.
func (g *graph) hasKey(ctx context.Context, requestID string) bool {
	GinkgoHelper()
	_, err := g.s.Queries().GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{OrgID: g.org, RequestID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	Expect(err).NotTo(HaveOccurred())
	return true
}

// leasesOn counts the leases on a node, all and live.
func (g *graph) leasesOn(ctx context.Context, ref string) (all, live int) {
	GinkgoHelper()
	Expect(g.s.Pool().QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE released_at IS NULL AND expires_at > now()) FROM leases WHERE org_id = $1 AND node_id = $2`,
		g.org, g.id(ref)).Scan(&all, &live)).To(Succeed())
	return all, live
}

// searchNodes runs the lexical search the schema indexes, scoped to the
// organization the way every query is. Search has no domain operation of
// its own yet; this is the read path as the schema defines it.
func (g *graph) searchNodes(ctx context.Context, query string) []string {
	GinkgoHelper()
	rows, err := g.s.Pool().Query(ctx,
		`SELECT id FROM nodes WHERE org_id = $1 AND node_search_document(title, body) @@ plainto_tsquery('english', $2) ORDER BY id`,
		g.org, query)
	Expect(err).NotTo(HaveOccurred())
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return ids
}

// leasedButNotReady counts the live leases held on nodes an open blocker
// keeps out of the ready set: the state a claim crossing a blocker must
// never leave behind.
func (g *graph) leasedButNotReady(ctx context.Context) int {
	GinkgoHelper()
	var n int
	Expect(g.s.Pool().QueryRow(ctx, `
		SELECT count(*)
		FROM leases l
		WHERE l.org_id = $1 AND l.released_at IS NULL AND l.expires_at > now()
		  AND EXISTS (
		    SELECT 1 FROM edges e
		    JOIN nodes b ON b.org_id = e.org_id AND b.id = e.from_node
		    WHERE e.org_id = l.org_id AND e.to_node = l.node_id AND e.kind = 'blocks' AND b.status = 'open')`,
		g.org).Scan(&n)).To(Succeed())
	return n
}

var _ = Describe("Concurrency and failure", Label(testsupport.DBLabel), func() {
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

	It("Opposite edges cannot both commit", func(ctx SpecContext) {
		By("given nodes A and B")
		g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))

		By("when two concurrent patches add an edge from A to B and from B to A")
		release := g.holdLock(ctx)
		ab := g.applyAsync(ctx, actor, "a-to-b", addEdge("#1", "#2"))
		ba := g.applyAsync(ctx, actor, "b-to-a", addEdge("#2", "#1"))
		Consistently(ab, 300*time.Millisecond).ShouldNot(Receive())
		Consistently(ba, 300*time.Millisecond).ShouldNot(Receive())
		release()
		var abErr, baErr error
		Eventually(ab, 5*time.Second).Should(Receive(&abErr))
		Eventually(ba, 5*time.Second).Should(Receive(&baErr))

		By("then exactly one commits")
		outcomes := []error{abErr, baErr}
		Expect(outcomes).To(ContainElement(BeNil()))
		Expect(outcomes).To(ContainElement(Not(BeNil())))
		Expect(g.countEdges(ctx)).To(Equal(1))
		Expect(g.countEvents(ctx, "a-to-b") + g.countEvents(ctx, "b-to-a")).To(Equal(1))
		for _, err := range outcomes {
			if err != nil {
				Expect(asCycle(err)).NotTo(BeNil())
			}
		}

		By("and the graph is acyclic")
		Expect(g.acyclic(ctx, "#1", "#2")).To(BeTrue())
	})

	It("One winner among many claimants", func(ctx SpecContext) {
		By("given exactly one ready node")
		g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))

		By("when 100 clients call ClaimNext concurrently")
		const claimants = 100
		results := make(chan *domain.Claimed, claimants)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range claimants {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				<-start
				c, err := domain.ClaimNext(ctx, s, pol, g.org, actor, claimReq(fmt.Sprintf("claim-%d", i), 0))
				Expect(err).NotTo(HaveOccurred())
				results <- c
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		By("then exactly one lease exists and 99 callers see no work")
		var won, empty int
		for c := range results {
			if c == nil {
				empty++
				continue
			}
			won++
			Expect(c.Node.Number).To(Equal(int64(1)))
		}
		Expect(won).To(Equal(1))
		Expect(empty).To(Equal(claimants - 1))
		var leases int
		Expect(s.Pool().QueryRow(ctx, `SELECT count(*) FROM leases WHERE org_id = $1`, g.org).Scan(&leases)).To(Succeed())
		Expect(leases).To(Equal(1))
		Expect(g.liveLeases(ctx)).To(Equal(1))
	})

	It("A fenced write rejects a stale attempt", func(ctx SpecContext) {
		By("given attempt 1 expired and attempt 2 claimed the node")
		g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
		worker := g.agent(ctx)
		first := g.mustClaim(ctx, worker, claimReq("attempt-1", time.Minute))
		g.age(ctx, first.Lease.ID, 2*time.Minute, -time.Minute)
		second := g.mustClaim(ctx, worker, claimReq("attempt-2", time.Minute))
		Expect(second.Node.ID).To(Equal(first.Node.ID))
		Expect(second.Lease.ID).NotTo(Equal(first.Lease.ID))
		node, lease, events := g.getNode(ctx, "#1"), g.getLease(ctx, second.Lease.ID), g.nodeEvents(ctx, "#1")

		By("when attempt 1 heartbeats, patches, or completes")
		_, err := domain.Heartbeat(ctx, s, pol, g.org, worker, "stale-heartbeat", first.Lease.ID, time.Hour)
		heartbeat := err
		_, err = g.applyWithLease(ctx, worker, "stale-patch", first.Lease.ID, update("#1", "Late result"))
		patch := err
		_, err = domain.Release(ctx, s, pol, g.org, worker, "stale-complete", first.Lease.ID, domain.ReleaseComplete, "")
		complete := err

		By("then every such call is rejected")
		Expect(heartbeat).To(MatchError(domain.ErrLeaseNotLive))
		Expect(patch).To(MatchError(domain.ErrLeaseNotLive))
		Expect(complete).To(MatchError(domain.ErrLeaseNotLive))

		By("and the node is unchanged")
		Expect(g.getNode(ctx, "#1")).To(Equal(node))
		Expect(g.getLease(ctx, second.Lease.ID)).To(Equal(lease))
		Expect(*g.getLease(ctx, first.Lease.ID).ReleaseReason).To(Equal(domain.ReleaseExpired))
		Expect(g.nodeEvents(ctx, "#1")).To(Equal(events))
		for _, request := range []string{"stale-heartbeat", "stale-patch", "stale-complete"} {
			Expect(g.countEvents(ctx, request)).To(BeZero(), "request %s wrote an event", request)
			Expect(g.hasKey(ctx, request)).To(BeFalse(), "request %s stored a response", request)
		}
		e := g.explain(ctx, "#1")
		Expect(e.Reasons).To(HaveExactElements(BeReason(domain.ReasonLeased)))
		Expect(e.Reasons[0].Lease.ID).To(Equal(second.Lease.ID))
	})

	It("Claiming races a new blocker", func(ctx SpecContext) {
		// Each round is one race between a claim and a patch. The
		// interleaving is the scheduler's, so the rounds exercise both
		// orders and the ones in between; every round is judged on its
		// own.
		const rounds = 25
		for r := range rounds {
			By(fmt.Sprintf("given a node about to be claimed (round %d)", r))
			x, y := fmt.Sprintf("#%d", 2*r+1), fmt.Sprintf("#%d", 2*r+2)
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, int64(2*r+1), nil)
				g.node(ctx, tx, int64(2*r+2), nil)
				// The blocker-to-be is held, so it is not itself ready
				// and the only ready node is the one being raced for.
				g.lease(ctx, tx, y, time.Hour)
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{int64(2*r + 1)}))

			By("when a patch adds a blocker concurrently")
			var (
				claimed            *domain.Claimed
				claimErr, patchErr error
				wg                 sync.WaitGroup
			)
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				<-start
				claimed, claimErr = domain.Claim(ctx, s, pol, g.org, actor, claimReq(fmt.Sprintf("claim-%d", r), 0))
			}()
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				<-start
				_, patchErr = domain.Apply(ctx, s, pol, g.org, actor, domain.Patch{
					RequestID: fmt.Sprintf("block-%d", r), Project: "platform", Ops: []domain.Op{addEdge(y, x)},
				})
			}()
			close(start)
			wg.Wait()

			By("then either the claim wins and the patch is rejected, or the patch wins and the claim returns no work")
			Expect(claimErr).NotTo(HaveOccurred())
			if claimed != nil {
				Expect(claimed.Node.Number).To(Equal(int64(2*r + 1)))
				Expect(patchErr).To(MatchError(domain.ErrLeaseRequired), "round %d: the claim won, so the patch must have been rejected", r)
				Expect(g.hasEdge(ctx, y, x)).To(BeFalse())
			} else {
				Expect(patchErr).NotTo(HaveOccurred(), "round %d: the claim found no work, so the patch must have won", r)
				Expect(g.hasEdge(ctx, y, x)).To(BeTrue())
			}

			By("and no lease exists on a node that is not ready")
			e := g.explain(ctx, x)
			Expect(e.Reasons).To(HaveLen(1), "round %d: %v", r, e.Reasons)
			Expect(e.Reasons).NotTo(SatisfyAll(
				ContainElement(BeReason(domain.ReasonLeased)),
				ContainElement(BeReason(domain.ReasonBlocked)),
			))
		}

		// The rounds above leave the interleaving to chance. The two below
		// force each crossing: one operation is held at the point where it
		// has locked the node, and the other is run against it.
		By("given a node about to be claimed, with the claim holding the node's row lock")
		x, y := "#101", "#102"
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 101, nil)
			g.node(ctx, tx, 102, nil)
			g.lease(ctx, tx, y, time.Hour)
		})
		hold := newPauser("SKIP LOCKED")
		claimDone := make(chan *domain.Claimed, 1)
		go func() {
			defer GinkgoRecover()
			c, err := domain.Claim(ctx, tracedStore(ctx, s.Pool(), hold), pol, g.org, actor, claimReq("claim-locked", 0))
			Expect(err).NotTo(HaveOccurred())
			claimDone <- c
		}()
		Eventually(hold.reached, 5*time.Second).Should(BeClosed())

		By("when a patch adds a blocker before the claim commits")
		patchDone := g.applyAsync(ctx, actor, "block-locked", addEdge(y, x))
		Consistently(patchDone, 300*time.Millisecond).ShouldNot(Receive(), "the fence must wait for the claim")
		close(hold.release)

		By("then the claim wins and the patch is rejected")
		var c *domain.Claimed
		Eventually(claimDone, 5*time.Second).Should(Receive(&c))
		Expect(c).NotTo(BeNil())
		Expect(c.Node.Number).To(Equal(int64(101)))
		var patchErr error
		Eventually(patchDone, 5*time.Second).Should(Receive(&patchErr))
		Expect(patchErr).To(MatchError(domain.ErrLeaseRequired))
		Expect(g.hasEdge(ctx, y, x)).To(BeFalse())
		Expect(g.explain(ctx, x).Reasons).To(HaveExactElements(BeReason(domain.ReasonLeased)))

		By("given a node about to be claimed, with a patch holding the node's row lock")
		x, y = "#103", "#104"
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 103, nil)
			g.node(ctx, tx, 104, nil)
			g.lease(ctx, tx, y, time.Hour)
		})
		hold = newPauser(domain.LockNodeSQL)
		patchFirst := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			_, err := domain.Apply(ctx, tracedStore(ctx, s.Pool(), hold), pol, g.org, actor, domain.Patch{
				RequestID: "block-first", Project: "platform", Ops: []domain.Op{addEdge(y, x)},
			})
			patchFirst <- err
		}()
		Eventually(hold.reached, 5*time.Second).Should(BeClosed())

		By("when a claim arrives before the patch commits")
		c, err := domain.Claim(ctx, s, pol, g.org, actor, claimReq("claim-second", 0))
		Expect(err).NotTo(HaveOccurred())
		close(hold.release)

		By("then the patch wins and the claim returns no work")
		Expect(c).To(BeNil())
		Eventually(patchFirst, 5*time.Second).Should(Receive(BeNil()))
		Expect(g.hasEdge(ctx, y, x)).To(BeTrue())
		Expect(g.explain(ctx, x).Reasons).To(HaveExactElements(BeReason(domain.ReasonBlocked, 104)))

		By("and no lease exists on a node that is not ready")
		Expect(g.leasedButNotReady(ctx)).To(BeZero())
	})

	It("Idempotent retry after a lost response", func(ctx SpecContext) {
		By("given a patch and a claim whose responses were lost")
		patch := []domain.Op{create("a", "A"), {CreateNode: &domain.CreateNode{Alias: "b", Title: "B", BlockedBy: []string{"a"}}}}
		applied := g.mustApply(ctx, actor, "patch-1", patch...)
		Expect(applied.Replayed).To(BeFalse())
		claimed := g.mustClaim(ctx, actor, claimReq("claim-1", 0))
		Expect(claimed.Replayed).To(BeFalse())
		nodes, edges := g.countNodes(ctx), g.countEdges(ctx)
		patchEvents, claimEvents := g.countEvents(ctx, "patch-1"), g.countEvents(ctx, "claim-1")
		Expect(patchEvents).To(Equal(3))
		Expect(claimEvents).To(Equal(1))
		var leases int
		Expect(s.Pool().QueryRow(ctx, `SELECT count(*) FROM leases WHERE org_id = $1`, g.org).Scan(&leases)).To(Succeed())
		Expect(leases).To(Equal(1))

		By("when each is retried with the same idempotency key")
		appliedAgain, err := g.apply(ctx, actor, "patch-1", patch...)
		Expect(err).NotTo(HaveOccurred())
		claimedAgain := g.mustClaim(ctx, actor, claimReq("claim-1", 0))

		By("then the original results are returned")
		Expect(appliedAgain.Replayed).To(BeTrue())
		appliedAgain.Replayed = false
		Expect(appliedAgain).To(Equal(applied))
		Expect(claimedAgain.Replayed).To(BeTrue())
		claimedAgain.Replayed = false
		Expect(claimedAgain).To(Equal(claimed))

		By("and no duplicate rows or events exist")
		Expect(g.countNodes(ctx)).To(Equal(nodes))
		Expect(g.countEdges(ctx)).To(Equal(edges))
		Expect(g.countEvents(ctx, "patch-1")).To(Equal(patchEvents))
		Expect(g.countEvents(ctx, "claim-1")).To(Equal(claimEvents))
		Expect(s.Pool().QueryRow(ctx, `SELECT count(*) FROM leases WHERE org_id = $1`, g.org).Scan(&leases)).To(Succeed())
		Expect(leases).To(Equal(1))
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(BeEmpty(), "the retried claim took a second node")
	})

	It("Restart leaves no torn state", func(ctx SpecContext) {
		killer := &backendKiller{admin: s.Pool()}
		killed := tracedStore(ctx, s.Pool(), killer)
		reaper := g.principalOf(ctx, "service")
		var next atomic.Int64
		fresh := func() int64 { return next.Add(1) }

		cases := []tornCase{
			{
				name:   "claim",
				before: leaseState{},
				after:  leaseState{Leases: 1, Live: 1, Events: 1, Key: true},
				arm: func() (func(*store.Store) error, func() any) {
					ref, label := g.labelled(ctx, fresh())
					req := claimReq("claim-"+label, 0)
					req.Labels = []string{label}
					run := func(through *store.Store) error {
						_, err := domain.Claim(ctx, through, pol, g.org, actor, req)
						return err
					}
					observe := func() any {
						all, live := g.leasesOn(ctx, ref)
						return leaseState{Leases: all, Live: live, Events: g.countEvents(ctx, req.RequestID), Key: g.hasKey(ctx, req.RequestID)}
					}
					return run, observe
				},
			},
			{
				name:   "patch",
				before: patchState{},
				after:  patchState{Retitled: true, Edge: true, Events: 2, Key: true},
				arm: func() (func(*store.Store) error, func() any) {
					from, _ := g.labelled(ctx, fresh())
					to, label := g.labelled(ctx, fresh())
					p := domain.Patch{RequestID: "patch-" + label, Project: "platform", Ops: []domain.Op{update(to, "Retitled"), addEdge(from, to)}}
					run := func(through *store.Store) error {
						_, err := domain.Apply(ctx, through, pol, g.org, actor, p)
						return err
					}
					observe := func() any {
						return patchState{
							Retitled: g.getNode(ctx, to).Title == "Retitled", Edge: g.hasEdge(ctx, from, to),
							Events: g.countEvents(ctx, p.RequestID), Key: g.hasKey(ctx, p.RequestID),
						}
					}
					return run, observe
				},
			},
			{
				name:   "complete",
				before: releaseState{},
				after:  releaseState{Released: true, Reason: domain.ReleaseComplete, Events: 1, Key: true},
				arm: func() (func(*store.Store) error, func() any) {
					_, label := g.labelled(ctx, fresh())
					req := claimReq("claim-"+label, 0)
					req.Labels = []string{label}
					held := g.mustClaim(ctx, actor, req)
					requestID := "complete-" + label
					run := func(through *store.Store) error {
						_, err := domain.Release(ctx, through, pol, g.org, actor, requestID, held.Lease.ID, domain.ReleaseComplete, "")
						return err
					}
					observe := func() any {
						l := g.getLease(ctx, held.Lease.ID)
						state := releaseState{Released: l.ReleasedAt != nil, Events: g.countEvents(ctx, requestID), Key: g.hasKey(ctx, requestID)}
						if l.ReleaseReason != nil {
							state.Reason = *l.ReleaseReason
						}
						return state
					}
					return run, observe
				},
			},
			{
				name:   "reap",
				before: reapState{},
				after:  reapState{Released: 2, Events: 2},
				arm: func() (func(*store.Store) error, func() any) {
					// Leases a killed run left expired are reaped first, so
					// the run under test has exactly this pair to release.
					_, err := domain.ExpireLeases(ctx, s, pol, g.org, reaper, 100)
					Expect(err).NotTo(HaveOccurred())
					pair := make([]string, 0, 2)
					for range 2 {
						_, label := g.labelled(ctx, fresh())
						req := claimReq("claim-"+label, time.Minute)
						req.Labels = []string{label}
						held := g.mustClaim(ctx, actor, req)
						g.age(ctx, held.Lease.ID, 2*time.Minute, -time.Minute)
						pair = append(pair, held.Lease.ID)
					}
					run := func(through *store.Store) error {
						_, err := domain.ExpireLeases(ctx, through, pol, g.org, reaper, 100)
						return err
					}
					observe := func() any {
						var state reapState
						Expect(s.Pool().QueryRow(ctx, `
							SELECT count(*) FILTER (WHERE l.release_reason = 'expired'),
							       (SELECT count(*) FROM events e WHERE e.org_id = $1 AND e.op = $3 AND e.after->>'ID' = ANY($2::text[]))
							FROM leases l WHERE l.org_id = $1 AND l.id = ANY($2::text[])`,
							g.org, pair, domain.OpExpireLease).Scan(&state.Released, &state.Events)).To(Succeed())
						return state
					}
					return run, observe
				},
			},
		}

		for _, tc := range cases {
			By("when " + tc.name + " runs to completion")
			run, observe := tc.arm()
			Expect(observe()).To(Equal(tc.before))
			killer.arm(0)
			Expect(run(killed)).To(Succeed())
			Expect(observe()).To(Equal(tc.after))
			statements := killer.n.Load()
			Expect(statements).To(BeNumerically(">", 2), "%s should span begin, work, and commit", tc.name)

			By(fmt.Sprintf("when the service is killed during %s, before each of its %d statements", tc.name, statements))
			for at := int64(1); at <= statements; at++ {
				run, observe := tc.arm()
				Expect(observe()).To(Equal(tc.before))
				killer.arm(at)
				err := run(killed)

				By(fmt.Sprintf("then %s is observably either fully applied or not applied (statement %d of %d)", tc.name, at, statements))
				if !killer.killed.Load() {
					// The run sent fewer statements than the clean one and
					// finished before the kill point.
					Expect(err).NotTo(HaveOccurred())
					Expect(observe()).To(Equal(tc.after))
					continue
				}
				Expect(err).To(HaveOccurred(), "%s killed before statement %d reported success", tc.name, at)
				Expect(observe()).To(Equal(tc.before), "%s killed before statement %d of %d left partial state", tc.name, at, statements)
			}
		}
	})

	It("Tenancy holds under every read path", func(ctx SpecContext) {
		By("given two organizations with similar data")
		// Both are named acme, both have a project called platform, and
		// both hold a node #1 with the same title, created under the same
		// request id, claimed, and carrying the same blob digest. The other
		// organization additionally holds what this one never sees.
		other := newGraph(ctx, s)
		otherActor := other.principal(ctx)
		for _, org := range []*graph{g, other} {
			a := actor
			if org == other {
				a = otherActor
			}
			org.mustApply(ctx, a, "setup", domain.Op{CreateNode: &domain.CreateNode{Alias: "search", Title: "Index the corpus", Body: "Build the search index"}})
			org.mustClaim(ctx, a, claimReq("claim", 0))
		}
		otherOnly := other.mustApply(ctx, otherActor, "other-only", domain.Op{CreateNode: &domain.CreateNode{Alias: "zebra", Title: "Zebra migration", Body: "Only the other organization knows about zebras"}})
		otherClaim := other.mustClaim(ctx, otherActor, claimReq("other-claim", 0))
		Expect(otherClaim.Node.Number).To(Equal(int64(2)))
		const shared, otherDigest = "sha256:" + "aa" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sha256:" + "bb" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		for _, blob := range []struct{ org, digest, backend string }{
			{g.org, shared, "acme-1"}, {other.org, shared, "acme-2"}, {other.org, otherDigest, "acme-2"},
		} {
			_, err := s.Queries().CreateBlob(ctx, store.CreateBlobParams{OrgID: blob.org, Digest: blob.digest, MediaType: "text/plain", Size: 1, Backend: blob.backend})
			Expect(err).NotTo(HaveOccurred())
		}
		otherNode, otherZebra := other.getNode(ctx, "#1"), other.getNode(ctx, "#2")
		Expect(otherNode.ID).NotTo(Equal(g.getNode(ctx, "#1").ID))

		By("then node references from one reveal neither content nor existence to the other")
		resolved, err := g.resolve(ctx, "platform#1")
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.ID).To(Equal(g.getNode(ctx, "#1").ID))
		resolved, err = g.resolve(ctx, "acme/platform#1")
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.ID).To(Equal(g.getNode(ctx, "#1").ID), "the shared name resolves to the caller's own organization")
		_, err = g.resolve(ctx, "platform#2")
		var notFound *domain.NotFoundError
		Expect(errors.As(err, &notFound)).To(BeTrue(), "expected not found, got %v", err)
		Expect(notFound.Missing).To(Equal(domain.MissingNode))
		_, err = g.resolve(ctx, other.org+"/platform#1")
		Expect(errors.As(err, &notFound)).To(BeTrue(), "expected not found, got %v", err)
		Expect(notFound.Missing).To(Equal(domain.MissingOrganization))
		_, err = s.Queries().GetNode(ctx, store.GetNodeParams{OrgID: g.org, ID: otherNode.ID})
		Expect(err).To(MatchError(pgx.ErrNoRows))
		_, err = s.Queries().GetProject(ctx, store.GetProjectParams{OrgID: g.org, ID: other.project})
		Expect(err).To(MatchError(pgx.ErrNoRows))
		_, err = g.applyPatch(ctx, actor, domain.Patch{RequestID: "cross", Project: other.project, Ops: []domain.Op{update("#1", "Reached across")}})
		Expect(err).To(MatchError(domain.ErrProjectNotFound))
		Expect(other.getNode(ctx, "#1").Title).To(Equal("Index the corpus"))

		By("and search from one reveals nothing of the other")
		Expect(g.searchNodes(ctx, "search index")).To(Equal([]string{g.getNode(ctx, "#1").ID}))
		Expect(g.searchNodes(ctx, "zebra")).To(BeEmpty())
		Expect(other.searchNodes(ctx, "zebra")).To(Equal([]string{otherZebra.ID}))

		By("and explain and the ready set from one reveal nothing of the other")
		Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := domain.Explain(ctx, tx, g.org, otherNode.ID)
			Expect(err).To(MatchError(domain.ErrNodeNotFound))
			_, err = domain.Explain(ctx, tx, g.org, otherZebra.ID)
			Expect(err).To(MatchError(domain.ErrNodeNotFound))
			nodes, err := domain.Ready(ctx, tx, g.org, domain.ReadyQuery{Projects: []string{other.project}})
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(BeEmpty(), "naming another organization's project widens nothing")
			return nil
		})).To(Succeed())
		e := other.explain(ctx, "#1")
		Expect(e.Reasons).To(HaveExactElements(BeReason(domain.ReasonLeased)))

		By("and events from one reveal nothing of the other")
		Expect(g.eventsFor(ctx, "other-only")).To(BeEmpty())
		Expect(g.eventsFor(ctx, "other-claim")).To(BeEmpty())
		for _, ev := range g.eventsFor(ctx, "setup") {
			Expect(ev.OrgID).To(Equal(g.org))
			Expect(*ev.NodeID).NotTo(Equal(otherNode.ID))
		}
		events, err := s.Queries().ListEventsForNode(ctx, store.ListEventsForNodeParams{OrgID: g.org, NodeID: &otherNode.ID, PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		Expect(events).To(BeEmpty())
		stream, err := s.Queries().ListEventsAfter(ctx, store.ListEventsAfterParams{OrgID: g.org, AfterID: "", PageSize: 1000})
		Expect(err).NotTo(HaveOccurred())
		Expect(stream).NotTo(BeEmpty())
		for _, ev := range stream {
			Expect(ev.OrgID).To(Equal(g.org))
		}
		Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			touched, err := domain.HumanTouched(ctx, tx, g.org, otherNode.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(touched).To(BeFalse())
			return nil
		})).To(Succeed())

		By("and blob digests from one reveal nothing of the other")
		blob, err := s.Queries().GetBlob(ctx, store.GetBlobParams{OrgID: g.org, Digest: shared})
		Expect(err).NotTo(HaveOccurred())
		Expect(blob.Backend).To(Equal("acme-1"))
		_, err = s.Queries().GetBlob(ctx, store.GetBlobParams{OrgID: g.org, Digest: otherDigest})
		Expect(err).To(MatchError(pgx.ErrNoRows))

		By("and neither leases nor idempotency keys cross over")
		_, err = s.Queries().GetLease(ctx, store.GetLeaseParams{OrgID: g.org, ID: otherClaim.Lease.ID})
		Expect(err).To(MatchError(pgx.ErrNoRows))
		_, err = domain.Heartbeat(ctx, s, pol, g.org, actor, "borrowed", otherClaim.Lease.ID, 0)
		Expect(err).To(MatchError(domain.ErrLeaseNotLive), "another organization's lease is indistinguishable from none")
		replay := g.mustApply(ctx, actor, "other-only", domain.Op{CreateNode: &domain.CreateNode{Alias: "zebra", Title: "Zebra migration", Body: "Only the other organization knows about zebras"}})
		Expect(replay.Replayed).To(BeFalse(), "another organization's request id replays nothing here")
		Expect(replay.Nodes[0].ID).NotTo(Equal(otherOnly.Nodes[0].ID))
	})
})
