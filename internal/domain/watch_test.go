package domain_test

import (
	"context"
	"fmt"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// stream is one WatchEvents call running in the background, with what
// it sent so far on msgs.
type stream struct {
	msgs   chan domain.WatchEvent
	cancel context.CancelFunc
	done   chan error
}

// watch opens a stream for actor over svc. It is stopped when the spec
// ends if the spec has not stopped it.
func (g *graph) watch(ctx context.Context, svc *domain.Service, actor domain.Actor, req domain.WatchRequest) *stream {
	GinkgoHelper()
	sctx, cancel := context.WithCancel(ctx)
	s := &stream{msgs: make(chan domain.WatchEvent, 1024), cancel: cancel, done: make(chan error, 1)}
	go func() {
		defer GinkgoRecover()
		s.done <- svc.WatchEvents(sctx, g.org, actor, req, func(ev domain.WatchEvent) error {
			select {
			case s.msgs <- ev:
				return nil
			case <-sctx.Done():
				return sctx.Err()
			}
		})
	}()
	DeferCleanup(func() { _ = s.stop() })
	return s
}

// stop ends the stream and returns what WatchEvents returned.
func (s *stream) stop() error {
	GinkgoHelper()
	s.cancel()
	select {
	case err := <-s.done:
		s.done <- err
		return err
	case <-time.After(10 * time.Second):
		Fail("stream did not stop within 10s")
		return nil
	}
}

// next is the next message, position-only ones included.
func (s *stream) next() domain.WatchEvent {
	GinkgoHelper()
	var msg domain.WatchEvent
	Eventually(s.msgs, 10*time.Second).Should(Receive(&msg), "the stream sent nothing")
	return msg
}

// nextEvent is the next message carrying an event, with its cursor.
func (s *stream) nextEvent() (domain.Event, string) {
	GinkgoHelper()
	for {
		msg := s.next()
		if msg.Event != nil {
			return *msg.Event, msg.Cursor
		}
	}
}

// quiet asserts the stream sends no event for a while.
func (s *stream) quiet() {
	GinkgoHelper()
	Consistently(func() bool {
		select {
		case msg := <-s.msgs:
			return msg.Event == nil
		default:
			return true
		}
	}, 500*time.Millisecond).Should(BeTrue(), "the stream sent an event it should not have")
}

// beginEvent opens a transaction that has written one event, with the
// given id, and leaves it open: the caller commits it, or does not. It
// is how a spec holds a transaction in flight past a later commit.
func (g *graph) beginEvent(ctx context.Context, actor domain.Actor, id string) pgx.Tx {
	GinkgoHelper()
	tx, err := g.s.Pool().Begin(ctx)
	Expect(err).NotTo(HaveOccurred())
	_, err = store.New(tx).CreateEvent(ctx, store.CreateEventParams{
		ID: id, OrgID: g.org, ProjectID: &g.project,
		PrincipalID: actor.PrincipalID, PrincipalKind: actor.PrincipalKind, IdentityScheme: actor.IdentityScheme,
		Op: "spec_event", PolicyVersion: pol.Version(),
	})
	Expect(err).NotTo(HaveOccurred())
	return tx
}

// listeners counts the backends of this database that are listening on
// the events channel, which is how a spec observes the listener from
// outside the process.
func (g *graph) listeners(ctx context.Context) int {
	GinkgoHelper()
	var n int
	Expect(g.s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND query = 'LISTEN skift_events'`,
	).Scan(&n)).To(Succeed())
	return n
}

func strPtr(s string) *string { return &s }

func eventIDs(events []domain.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

var _ = Describe("Change stream", Label(testsupport.DBLabel), func() {
	var (
		s     *store.Store
		g     *graph
		svc   *domain.Service
		actor domain.Actor
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		svc = domain.NewService(s.Pool(), pol)
		actor = g.principal(ctx)
	})

	Describe("Scenarios", func() {
		It("A late commit is not skipped", func(ctx SpecContext) {
			By("given a consumer watching from now")
			consumer := g.watch(ctx, svc, actor, domain.WatchRequest{})
			Expect(consumer.next().Event).To(BeNil(), "a stream from now opens with its position")

			By("and transaction A allocates an event id and pauses")
			idA := domain.NewULID()
			txA := g.beginEvent(ctx, actor, idA)

			By("and transaction B allocates a higher id and commits")
			idB := domain.NewULID()
			Expect(idB > idA).To(BeTrue())
			txB := g.beginEvent(ctx, actor, idB)
			Expect(txB.Commit(ctx)).To(Succeed())

			By("and a consumer reads through B's position")
			evB, afterB := consumer.nextEvent()
			Expect(evB.ID).To(Equal(idB))

			By("when A commits")
			Expect(txA.Commit(ctx)).To(Succeed())

			By("then the consumer receives A's event")
			evA, _ := consumer.nextEvent()
			Expect(evA.ID).To(Equal(idA))

			By("and so does a consumer resuming from B's position")
			resumed := g.watch(ctx, svc, actor, domain.WatchRequest{Cursor: afterB})
			ev, _ := resumed.nextEvent()
			Expect(ev.ID).To(Equal(idA))
			resumed.quiet()
		})

		It("A consumer resumes after a restart", func(ctx SpecContext) {
			By("given a consumer that processed 100 events and stopped")
			consumer := g.watch(ctx, svc, actor, domain.WatchRequest{})
			Expect(consumer.next().Event).To(BeNil())
			var ids []string
			for batch := 0; batch < 4; batch++ {
				ops := make([]domain.Op, 0, 25)
				for i := 0; i < 25; i++ {
					ops = append(ops, create(fmt.Sprintf("n%d_%d", batch, i), fmt.Sprintf("Node %d/%d", batch, i)))
				}
				ids = append(ids, eventIDs(g.mustApply(ctx, actor, fmt.Sprintf("batch-%d", batch), ops...).Events)...)
			}
			Expect(ids).To(HaveLen(100))
			var stored string
			for i := range 100 {
				ev, cursor := consumer.nextEvent()
				Expect(ev.ID).To(Equal(ids[i]), "event %d arrives in order", i+1)
				stored = cursor
			}
			Expect(consumer.stop()).To(MatchError(context.Canceled))

			By("and events 101 onward committed while it was away")
			later := eventIDs(g.mustApply(ctx, actor, "later", create("x", "Node 101"), create("y", "Node 102")).Events)
			Expect(later).To(HaveLen(2))

			By("when it reconnects with its stored cursor")
			resumed := g.watch(ctx, svc, actor, domain.WatchRequest{Cursor: stored})

			By("then it receives event 101 onward and no earlier event")
			ev, _ := resumed.nextEvent()
			Expect(ev.ID).To(Equal(later[0]))
			ev, _ = resumed.nextEvent()
			Expect(ev.ID).To(Equal(later[1]))
			resumed.quiet()
		})

		It("Missing a notification does not lose an event", func(ctx SpecContext) {
			By("given a consumer connecting exactly as a transaction commits")
			// Its cursor comes from an earlier stream, closed since, so
			// nothing was listening when the commit's notification fired.
			earlier := g.watch(ctx, svc, actor, domain.WatchRequest{})
			cursor := earlier.next().Cursor
			Expect(earlier.stop()).To(MatchError(context.Canceled))
			Eventually(func() int { return g.listeners(ctx) }).Should(BeZero(), "the last stream out takes the listener with it")

			id := domain.NewULID()
			tx := g.beginEvent(ctx, actor, id)
			committed := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				committed <- tx.Commit(ctx)
			}()
			consumer := g.watch(ctx, svc, actor, domain.WatchRequest{Cursor: cursor})
			Eventually(committed).Should(Receive(Succeed()))

			By("then a durable re-query after listening still returns the event")
			ev, _ := consumer.nextEvent()
			Expect(ev.ID).To(Equal(id))
		})
	})

	It("issues an opaque cursor that is not the event id, and refuses one it did not issue", func(ctx SpecContext) {
		consumer := g.watch(ctx, svc, actor, domain.WatchRequest{})
		Expect(consumer.next().Event).To(BeNil())
		g.mustApply(ctx, actor, "one", create("a", "A"))
		ev, cursor := consumer.nextEvent()
		Expect(cursor).NotTo(BeEmpty())
		Expect(cursor).NotTo(Equal(ev.ID))
		Expect(cursor).NotTo(ContainSubstring(ev.ID))

		discard := func(domain.WatchEvent) error { return nil }
		for _, bad := range []string{ev.ID, "v1.not-base64!", "v0." + cursor[3:], "v1.e30"} {
			err := svc.WatchEvents(ctx, g.org, actor, domain.WatchRequest{Cursor: bad}, discard)
			Expect(err).To(MatchError(domain.ErrCursorInvalid), bad)
		}

		By("while a seen event at exactly the cursor's transaction boundary is within it")
		resumed := g.watch(ctx, svc, actor, domain.WatchRequest{Cursor: domain.SeenAtHorizon(cursor)})
		Consistently(resumed.done, 300*time.Millisecond).ShouldNot(Receive())
		g.mustApply(ctx, actor, "two", create("b", "B"))
		ev, _ = resumed.nextEvent()
		Expect(ev.Op).To(Equal(domain.OpCreateNode))
	})

	It("rejects a cursor older than the replay window, and accepts one inside it", func(ctx SpecContext) {
		consumer := g.watch(ctx, svc, actor, domain.WatchRequest{})
		cursor := consumer.next().Cursor
		Expect(consumer.stop()).To(MatchError(context.Canceled))

		start := domain.ReplayWindowStart(time.Now())
		expired := domain.AgeCursor(cursor, start.Add(-time.Second))
		err := svc.WatchEvents(ctx, g.org, actor, domain.WatchRequest{Cursor: expired}, func(domain.WatchEvent) error { return nil })
		Expect(err).To(MatchError(domain.ErrCursorExpired))

		retained := g.watch(ctx, svc, actor, domain.WatchRequest{Cursor: domain.AgeCursor(cursor, start)})
		Consistently(retained.done, 300*time.Millisecond).ShouldNot(Receive())
		g.mustApply(ctx, actor, "one", create("a", "A"))
		ev, _ := retained.nextEvent()
		Expect(ev.Op).To(Equal(domain.OpCreateNode))
	})

	It("establishes the listener, then reads durable state, then waits", func(ctx SpecContext) {
		Expect(g.listeners(ctx)).To(BeZero())
		consumer := g.watch(ctx, svc, actor, domain.WatchRequest{})

		By("the opening position arrives from a stream that is already listening")
		Expect(consumer.next().Event).To(BeNil())
		Expect(g.listeners(ctx)).To(Equal(1))

		By("and the stream waits, sending nothing, until something commits")
		consumer.quiet()
		g.mustApply(ctx, actor, "one", create("a", "A"))
		ev, _ := consumer.nextEvent()
		Expect(ev.Op).To(Equal(domain.OpCreateNode))

		By("and a second stream shares the one listener")
		second := g.watch(ctx, svc, actor, domain.WatchRequest{})
		Expect(second.next().Event).To(BeNil())
		Expect(g.listeners(ctx)).To(Equal(1))

		By("and the last stream out takes the listener with it")
		Expect(consumer.stop()).To(MatchError(context.Canceled))
		Expect(second.stop()).To(MatchError(context.Canceled))
		Eventually(func() int { return g.listeners(ctx) }).Should(BeZero())
	})

	It("advances the cursor through an interval the policy filtered entirely", func(ctx SpecContext) {
		By("given a policy that lets only users watch, and an agent watching under it")
		usersOnly, err := policy.Load(ctx, fstest.MapFS{"narrow.rego": &fstest.MapFile{Data: []byte(`
package skift.authz

default allow := false

allow if {
	input.action == "watch"
	input.principal.kind == "user"
}

decision := {"allow": allow, "reasons": []}
`)}})
		Expect(err).NotTo(HaveOccurred())
		narrow := domain.NewService(s.Pool(), usersOnly)
		agent := g.agent(ctx)
		consumer := g.watch(ctx, narrow, agent, domain.WatchRequest{})
		opening := consumer.next()
		Expect(opening.Event).To(BeNil())

		By("when events the agent may not see are committed")
		hidden := eventIDs(g.mustApply(ctx, actor, "hidden", create("a", "A"), create("b", "B"), create("c", "C")).Events)

		By("then the agent is handed a position past them and no event")
		advanced := consumer.next()
		Expect(advanced.Event).To(BeNil())
		Expect(advanced.Cursor).NotTo(Equal(opening.Cursor))
		consumer.quiet()

		By("and a user resuming from that position sees nothing before it")
		user := g.watch(ctx, svc, actor, domain.WatchRequest{Cursor: advanced.Cursor})
		shown := eventIDs(g.mustApply(ctx, actor, "shown", create("d", "D")).Events)
		ev, _ := user.nextEvent()
		Expect(ev.ID).To(Equal(shown[0]))
		Expect(hidden).NotTo(ContainElement(ev.ID))
		user.quiet()
	})

	It("narrows the stream to a project and to operations", func(ctx SpecContext) {
		g.newProject(ctx, "other")
		byProject := g.watch(ctx, svc, actor, domain.WatchRequest{Project: "other"})
		Expect(byProject.next().Event).To(BeNil())
		byOp := g.watch(ctx, svc, actor, domain.WatchRequest{Ops: []string{domain.OpUpdateNode}})
		Expect(byOp.next().Event).To(BeNil())

		g.mustApply(ctx, actor, "platform-create", create("a", "A"))
		inOther := g.applyIn(ctx, actor, "other", "other-create", create("b", "B")).Events
		g.mustApply(ctx, actor, "platform-update", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#1", Title: strPtr("A renamed")}})

		ev, _ := byProject.nextEvent()
		Expect(ev.ID).To(Equal(inOther[0].ID))
		byProject.quiet()

		ev, _ = byOp.nextEvent()
		Expect(ev.Op).To(Equal(domain.OpUpdateNode))
		byOp.quiet()
	})

	It("refuses a project it cannot resolve", func(ctx SpecContext) {
		err := svc.WatchEvents(ctx, g.org, actor, domain.WatchRequest{Project: "nowhere"}, func(domain.WatchEvent) error { return nil })
		Expect(err).To(MatchError(domain.ErrProjectNotFound))
	})
})
