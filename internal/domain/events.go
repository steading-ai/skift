package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
)

// Every mutation writes one event, in the transaction that made it, so the
// log and the graph cannot disagree. The actor on each event is whatever
// the transport resolved; a patch body has no field for it, and nothing
// here reads one. See the Event section of docs/domain-model.md and
// docs/adrs/006-provenance-not-authority.md.

// Errors a patch is rejected with before any operation runs.
var (
	// ErrNoActor: an event names its actor, so a patch cannot be applied
	// without one. The transport resolves it; a caller passing a zero
	// Actor has lost it somewhere.
	ErrNoActor = errors.New("a patch needs a transport-resolved actor")
	// ErrReasonRequired: a transition that carries a reason was given
	// none.
	ErrReasonRequired = errors.New("a reason is required")
)

// Actor is the principal applying a patch, as the transport resolved it.
// Every event the patch writes records it verbatim.
type Actor struct {
	PrincipalID string
	// PrincipalKind is `user`, `agent`, or `service`.
	PrincipalKind string
	// IdentityScheme is the transport identity the actor arrived with:
	// `unix`, `tailscale`, `oidc`, or `token`.
	IdentityScheme string
}

func (a Actor) validate() error {
	if a.PrincipalID == "" || a.PrincipalKind == "" || a.IdentityScheme == "" {
		return ErrNoActor
	}
	return nil
}

// requiresReason reports whether an operation is a transition that must
// say why. Dropping, reopening, and parking are assertions about the work
// rather than edits to it, and the log is where that assertion lives. A
// hold nobody can explain is also one nobody dares release.
func requiresReason(op string) bool {
	switch op {
	case OpDropNode, OpReopenNode, OpHoldNode, OpUnholdNode, OpHoldProject, OpUnholdProject:
		return true
	}
	return false
}

// recorder writes the events of one transaction for one actor, and keeps
// them in the order written. request is nil for writes no client asked
// for, such as the expiry timer releasing a lease. lease is the lease the
// request carries, when it carries one.
type recorder struct {
	tx      store.Tx
	orgID   string
	actor   Actor
	policy  *policy.Bundle
	request *string
	lease   *store.Lease
	events  []store.Event
}

// event authorizes one operation and writes its record. Authorization
// sits here because every mutation writes an event and nothing else
// does, so a mutation the policy has not allowed cannot be committed.
// reason is nil for operations that carry none. before and after are the
// typed change, encoded as JSON; nil leaves the column null.
func (r *recorder) event(ctx context.Context, op, nodeID, projectID string, reason *string, before, after any) error {
	if requiresReason(op) && (reason == nil || *reason == "") {
		return fmt.Errorf("%s: %w", op, ErrReasonRequired)
	}
	version, err := r.authorize(ctx, op, nodeID, projectID, before, after)
	if err != nil {
		return err
	}
	beforeJSON, err := encodeChange(before)
	if err != nil {
		return err
	}
	afterJSON, err := encodeChange(after)
	if err != nil {
		return err
	}
	ev, err := r.tx.CreateEvent(ctx, store.CreateEventParams{
		ID: newULID(), OrgID: r.orgID, ProjectID: &projectID, NodeID: &nodeID,
		PrincipalID: r.actor.PrincipalID, PrincipalKind: r.actor.PrincipalKind,
		IdentityScheme: r.actor.IdentityScheme, RequestID: r.request,
		Op: op, Reason: reason, Before: beforeJSON, After: afterJSON, PolicyVersion: version,
	})
	if err != nil {
		return fmt.Errorf("record %s event: %w", op, err)
	}
	r.events = append(r.events, ev)
	return nil
}

// projectEvent authorizes a project operation and writes its event,
// through the same actor, policy and request this transaction's other
// events use. reason is required for the operations [requiresReason]
// names, as it is for a node's.
func (r *recorder) projectEvent(ctx context.Context, op string, reason *string, before, after any) error {
	return projectEvent(ctx, r.tx, r.policy, r.orgID, r.actor, r.request, op, reason, before, after)
}

func encodeChange(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode event change: %w", err)
	}
	return encoded, nil
}

// EventsForRequest returns every event one patch wrote, in the order its
// operations were applied. Event ids are minted in sequence as the patch
// runs, so id order is operation order.
func EventsForRequest(ctx context.Context, tx store.Tx, orgID, requestID string) ([]store.Event, error) {
	events, err := tx.ListEventsForRequest(ctx, store.ListEventsForRequestParams{OrgID: orgID, RequestID: &requestID})
	if err != nil {
		return nil, fmt.Errorf("read events for request %q: %w", requestID, err)
	}
	return events, nil
}

// HumanTouched reports whether a user principal has ever written to the
// node. It is derived from the node's events on every call and stored
// nowhere, so it cannot drift from the log and cannot be asserted by a
// client.
func HumanTouched(ctx context.Context, tx store.Tx, orgID, nodeID string) (bool, error) {
	touched, err := tx.NodeHumanTouched(ctx, store.NodeHumanTouchedParams{OrgID: orgID, NodeID: &nodeID})
	if err != nil {
		return false, fmt.Errorf("derive human_touched for node %s: %w", nodeID, err)
	}
	return touched, nil
}
