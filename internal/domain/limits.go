package domain

import (
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"time"
)

// Fixed structural ceilings. See the Limits section of docs/domain-model.md.
//
// These are not rate limits. Each one stops a single request from consuming
// the service, needs no workload data to set, and is checked before the
// request does any work. Per-principal rate limiting is deliberately
// absent; the interceptor is the seam for it. See
// docs/adrs/006-provenance-not-authority.md.
//
// The statement timeout in the same table is the store's: every
// transaction [store.Store.WithTx] opens runs under it.

// Limits are the ceilings in force. Zero is not a valid value for any of
// them: a ceiling that is off is not a ceiling.
type Limits struct {
	// OpsPerPatch bounds the operations one patch carries, so one
	// transaction cannot hold its locks unboundedly.
	OpsPerPatch int
	// NodesPerPatch bounds the nodes one patch creates.
	NodesPerPatch int
	// EdgesPerPatch bounds the edges one patch adds, counting each
	// blocked_by on a create_node as one. It bounds cycle-check work.
	EdgesPerPatch int
	// BodyBytes bounds a node body.
	BodyBytes int
	// ResultBytes bounds a result body.
	ResultBytes int
	// CommentBytes bounds a comment body.
	CommentBytes int
	// InlinePartBytes bounds one inline text part of an artifact.
	InlinePartBytes int
	// BlobBytes bounds one managed blob. Dagger is not a data transport.
	BlobBytes int64
	// LeaseTTL is the longest a claim or heartbeat can hold a node for
	// without renewing. A longer request is clamped to it rather than
	// refused, since the holder can always renew.
	LeaseTTL time.Duration
	// PageSize bounds one page of any list. A read that asks for no
	// particular size gets a page this large.
	PageSize int
	// KeepSet bounds what one focus sweep may name as staying live, so a
	// single call cannot rewrite an organization unboundedly.
	KeepSet int
}

// Default ceilings, high enough that ordinary work never meets them.
const (
	MaxOpsPerPatch     = 1000
	MaxNodesPerPatch   = 1000
	MaxEdgesPerPatch   = 2000
	MaxBodyBytes       = 256 << 10
	MaxResultBytes     = 256 << 10
	MaxCommentBytes    = 64 << 10
	MaxInlinePartBytes = 256 << 10
	MaxBlobBytes       = 64 << 20
	MaxLeaseTTL        = time.Hour
	MaxPageSize        = 1000
	MaxKeepSet         = 1000
)

// DefaultLimits are the ceilings in force until [SetLimits] replaces them.
func DefaultLimits() Limits {
	return Limits{
		OpsPerPatch:     MaxOpsPerPatch,
		NodesPerPatch:   MaxNodesPerPatch,
		EdgesPerPatch:   MaxEdgesPerPatch,
		BodyBytes:       MaxBodyBytes,
		ResultBytes:     MaxResultBytes,
		CommentBytes:    MaxCommentBytes,
		InlinePartBytes: MaxInlinePartBytes,
		BlobBytes:       MaxBlobBytes,
		LeaseTTL:        MaxLeaseTTL,
		PageSize:        MaxPageSize,
		KeepSet:         MaxKeepSet,
	}
}

// Limit names a ceiling, as a [*LimitError] reports it.
type Limit string

// The ceilings, by name.
const (
	LimitOpsPerPatch     Limit = "operations per patch"
	LimitNodesPerPatch   Limit = "nodes per patch"
	LimitEdgesPerPatch   Limit = "edges per patch"
	LimitBodyBytes       Limit = "body bytes"
	LimitResultBytes     Limit = "result bytes"
	LimitCommentBytes    Limit = "comment bytes"
	LimitInlinePartBytes Limit = "inline part bytes"
	LimitBlobBytes       Limit = "blob bytes"
	LimitLeaseTTL        Limit = "lease ttl"
	LimitPageSize        Limit = "page size"
	LimitKeepSet         Limit = "focus keep-set size"
)

// LimitError reports a request that exceeds a ceiling: which one, what it
// allows, and what the request asked for. It is returned before any work
// is done.
type LimitError struct {
	Limit Limit
	// Max is the ceiling in force.
	Max int64
	// Value is what the request carried.
	Value int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s exceeds the limit: %d, at most %d allowed", e.Limit, e.Value, e.Max)
}

// exceeded is the [*LimitError] for value against limit, or nil when the
// ceiling holds.
func exceeded(limit Limit, value, max int64) error {
	if value <= max {
		return nil
	}
	return &LimitError{Limit: limit, Max: max, Value: value}
}

// ErrInvalidLimits: a ceiling was set to zero or below.
var ErrInvalidLimits = errors.New("every limit must be positive")

// validate rejects a Limits with any ceiling at zero or below. It
// reflects over the ceilings rather than listing them again, so one added
// to the struct cannot be forgotten here and silently default to zero,
// which is the one value that turns a ceiling off. Every ceiling is a
// count or a duration, which is why one integer test covers them all.
func (l Limits) validate() error {
	v := reflect.ValueOf(l)
	for i := range v.NumField() {
		if !v.Field(i).CanInt() || v.Field(i).Int() <= 0 {
			return ErrInvalidLimits
		}
	}
	return nil
}

// active holds the ceilings in force. It is read on every request and
// written once at startup, so it is a pointer swapped atomically rather
// than a value under a lock.
var active atomic.Pointer[Limits]

func init() {
	l := DefaultLimits()
	active.Store(&l)
}

// ActiveLimits returns the ceilings in force.
func ActiveLimits() Limits {
	return *active.Load()
}

// SetLimits replaces the ceilings in force, for the service to apply its
// configuration at startup. A Limits with any ceiling at zero or below is
// refused with [ErrInvalidLimits] and the ceilings in force are unchanged.
func SetLimits(l Limits) error {
	if err := l.validate(); err != nil {
		return err
	}
	active.Store(&l)
	return nil
}

// checkPatchShape rejects a patch whose shape exceeds a ceiling: too many
// operations, or too many nodes or edges created. It reads nothing but
// the patch, so it runs before the request id is claimed or a
// transaction opened.
func checkPatchShape(p Patch, l Limits) error {
	if err := exceeded(LimitOpsPerPatch, int64(len(p.Ops)), int64(l.OpsPerPatch)); err != nil {
		return err
	}
	var nodes, edges int64
	for _, op := range p.Ops {
		switch {
		case op.CreateNode != nil:
			nodes++
			edges += int64(len(op.CreateNode.BlockedBy))
		case op.AddEdge != nil:
			edges++
		}
	}
	if err := exceeded(LimitNodesPerPatch, nodes, int64(l.NodesPerPatch)); err != nil {
		return err
	}
	return exceeded(LimitEdgesPerPatch, edges, int64(l.EdgesPerPatch))
}

// boundPageSize applies the page ceiling to a requested page size. Zero
// or less asks for no particular size and gets the ceiling; more than the
// ceiling is a [*LimitError].
func boundPageSize(requested int, l Limits) (int, error) {
	if requested <= 0 {
		return l.PageSize, nil
	}
	if err := exceeded(LimitPageSize, int64(requested), int64(l.PageSize)); err != nil {
		return 0, err
	}
	return requested, nil
}
