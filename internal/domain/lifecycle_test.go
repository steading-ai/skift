package domain_test

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// A node's lifecycle, as a state machine checked against a model. The
// model is the rules of docs/domain-model.md for one root node with no
// edges: three statuses, a hold that is an eligibility axis rather than
// a status, and a lease that fences every write while it lives. rapid
// drives random action sequences against the real service and, after
// each, the stored node must match the model, and the service must have
// accepted or refused the action as the model says.
type lifecycle struct {
	h *history
	// The model.
	status string
	held   bool
	live   bool
	// lease is the id of the last lease claimed, live or not, so that
	// renewing and releasing a dead lease is an action too.
	lease string
	seq   int
}

func newLifecycle(ctx context.Context, t *rapid.T, s *store.Store) *lifecycle {
	m := &lifecycle{h: newHistory(ctx, t, s), status: "open"}
	_, err := domain.Apply(ctx, s, pol, m.h.org, m.h.user, domain.Patch{RequestID: "seed", Project: "platform",
		Ops: []domain.Op{{CreateNode: &domain.CreateNode{Title: "The node"}}}})
	if err != nil {
		t.Fatalf("create the node: %v", err)
	}
	return m
}

func (m *lifecycle) requestID() string {
	m.seq++
	return fmt.Sprintf("step-%d", m.seq)
}

// apply runs one operation on the node as the user, carrying no lease,
// and tells the model what should have happened to it.
func (m *lifecycle) apply(t *rapid.T, op domain.Op, expect error, then func()) {
	_, err := domain.Apply(m.h.ctx, m.h.s, pol, m.h.org, m.h.user, domain.Patch{RequestID: m.requestID(), Project: "platform", Ops: []domain.Op{op}})
	m.h.logf("%s => %v", renderPatch(domain.Patch{Ops: []domain.Op{op}}), err)
	switch {
	case m.live:
		if !errors.Is(err, domain.ErrLeaseRequired) {
			t.Fatalf("a live lease fences every write: expected %v, got %v", domain.ErrLeaseRequired, err)
		}
	case expect != nil:
		if !errors.Is(err, expect) {
			t.Fatalf("expected %v, got %v", expect, err)
		}
	case err != nil:
		t.Fatalf("unexpected refusal: %v", err)
	default:
		then()
	}
}

func (m *lifecycle) claim(t *rapid.T) {
	c, err := domain.Claim(m.h.ctx, m.h.s, pol, m.h.org, m.h.agent, domain.ClaimRequest{
		RequestID: m.requestID(), Projects: []string{"platform"}, Harness: "spec", AgentRef: "spec",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ready := m.status == "open" && !m.held && !m.live
	m.h.logf("claim => %v (ready per model: %v)", c != nil, ready)
	if (c != nil) != ready {
		t.Fatalf("claim answered %v; the model says the node is ready: %v", c != nil, ready)
	}
	if c != nil {
		m.live, m.lease = true, c.Lease.ID
	}
}

func (m *lifecycle) heartbeat(t *rapid.T) {
	_, err := domain.Heartbeat(m.h.ctx, m.h.s, pol, m.h.org, m.h.agent, m.requestID(), m.lease, 0)
	m.h.logf("heartbeat => %v", err)
	m.expectLive(t, err)
}

func (m *lifecycle) release(t *rapid.T) {
	reason := rapid.SampledFrom([]string{domain.ReleaseGate, domain.ReleaseCrash, domain.ReleaseBudget, domain.ReleasePreempted}).Draw(t, "reason")
	_, err := domain.Release(m.h.ctx, m.h.s, pol, m.h.org, m.h.agent, m.requestID(), m.lease, reason, "")
	m.h.logf("release %s => %v", reason, err)
	m.expectLive(t, err)
	m.live = false
}

// expectLive: an action on the lease succeeds while it lives and is
// refused as not live once it has ended, however it ended.
func (m *lifecycle) expectLive(t *rapid.T, err error) {
	switch {
	case m.live && err != nil:
		t.Fatalf("the lease is live: %v", err)
	case !m.live && !errors.Is(err, domain.ErrLeaseNotLive):
		t.Fatalf("the lease is over: expected %v, got %v", domain.ErrLeaseNotLive, err)
	}
}

func (m *lifecycle) expire(t *rapid.T) {
	if _, err := m.h.s.Pool().Exec(m.h.ctx,
		`UPDATE leases SET claimed_at = now() - interval '2 minutes', expires_at = now() - interval '1 minute' WHERE org_id = $1 AND id = $2`,
		m.h.org, m.lease); err != nil {
		t.Fatalf("expire: %v", err)
	}
	m.h.logf("expire %s", m.lease)
	m.live = false
}

// finish is the holder completing its node under its lease: the one
// write a live lease admits, and the end of the lease.
func (m *lifecycle) finish(t *rapid.T) {
	_, err := domain.Apply(m.h.ctx, m.h.s, pol, m.h.org, m.h.agent, domain.Patch{
		RequestID: m.requestID(), Project: "platform", LeaseID: m.lease, Ops: []domain.Op{complete("#1", "done")},
	})
	m.h.logf("finish under %s => %v", m.lease, err)
	if err != nil {
		t.Fatalf("the holder completes its open node: %v", err)
	}
	m.status, m.live = "done", false
}

func (m *lifecycle) check(t *rapid.T) {
	node := m.h.getNodeStatus(t)
	live := m.h.liveLease(t)
	if node.status != m.status || node.held != m.held || live != m.live {
		t.Fatalf("stored node is status=%s held=%v live=%v; the model says status=%s held=%v live=%v",
			node.status, node.held, live, m.status, m.held, m.live)
	}
}

type nodeView struct {
	status string
	held   bool
}

func (h *history) getNodeStatus(t *rapid.T) nodeView {
	var v nodeView
	var heldAt *string
	if err := h.s.Pool().QueryRow(h.ctx, `SELECT status, held_at::text FROM nodes WHERE org_id = $1 AND number = 1`, h.org).Scan(&v.status, &heldAt); err != nil {
		t.Fatalf("read the node: %v", err)
	}
	v.held = heldAt != nil
	return v
}

func (h *history) liveLease(t *rapid.T) bool {
	var n int
	if err := h.s.Pool().QueryRow(h.ctx,
		`SELECT count(*) FROM leases WHERE org_id = $1 AND released_at IS NULL AND expires_at > now()`, h.org).Scan(&n); err != nil {
		t.Fatalf("read leases: %v", err)
	}
	return n > 0
}

var _ = Describe("Node lifecycle", Label(testsupport.DBLabel), func() {
	var s *store.Store

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
	})

	It("follows the model under any sequence of claims, holds, completions, drops and reopens", func(ctx SpecContext) {
		checkProperty(func(t *rapid.T) {
			m := newLifecycle(ctx, t, s)
			actions := map[string]func(*rapid.T){
				"":      m.check,
				"claim": m.claim,
				"complete": func(t *rapid.T) {
					var expect error
					if m.status != "open" {
						expect = domain.ErrStatusConflict
					}
					m.apply(t, complete("#1", "done by hand"), expect, func() { m.status = "done" })
				},
				"drop": func(t *rapid.T) {
					var expect error
					if m.status == "dropped" {
						expect = domain.ErrStatusConflict
					}
					m.apply(t, domain.Op{DropNode: &domain.DropNode{Node: "#1", Reason: domain.DropNotViable}}, expect, func() { m.status = "dropped" })
				},
				"reopen": func(t *rapid.T) {
					var expect error
					if m.status == "open" {
						expect = domain.ErrStatusConflict
					}
					m.apply(t, domain.Op{ReopenNode: &domain.ReopenNode{Node: "#1", Reason: "again"}}, expect, func() { m.status = "open" })
				},
				"hold": func(t *rapid.T) {
					var expect error
					if m.held {
						expect = domain.ErrStatusConflict
					}
					m.apply(t, domain.Op{HoldNode: &domain.HoldNode{Node: "#1", Reason: "parked"}}, expect, func() { m.held = true })
				},
				"unhold": func(t *rapid.T) {
					var expect error
					if !m.held {
						expect = domain.ErrNotHeld
					}
					m.apply(t, domain.Op{UnholdNode: &domain.UnholdNode{Node: "#1", Reason: "back"}}, expect, func() { m.held = false })
				},
			}
			// Lease actions need a lease to name; finishing needs a live one.
			actions["heartbeat"] = func(t *rapid.T) {
				if m.lease == "" {
					t.Skip("no lease yet")
				}
				m.heartbeat(t)
			}
			actions["release"] = func(t *rapid.T) {
				if m.lease == "" {
					t.Skip("no lease yet")
				}
				m.release(t)
			}
			actions["expire"] = func(t *rapid.T) {
				if !m.live {
					t.Skip("no live lease")
				}
				m.expire(t)
			}
			actions["finish"] = func(t *rapid.T) {
				if !m.live {
					t.Skip("no live lease")
				}
				m.finish(t)
			}
			t.Repeat(actions)
		})
	})
})
