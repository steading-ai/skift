package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/domain"
)

// The mapping between the skift.v1 wire types and the domain's, in one
// place. Requests become domain inputs field by field, so an output-only
// field a client sets has nothing to land on and is dropped here; responses
// are built from what the domain returns and nothing else. See the
// provenance note at the top of proto/skift/v1/skift.proto.

// defaultPageSize is the page a client gets when it asks for none. The
// domain bounds anything larger.
const defaultPageSize = 100

// Enumerations: the wire enum and the string the domain and the schema
// store, both ways. An unspecified enum maps to the empty string, which is
// the domain's "unset" and takes the domain's default.

var priorities = enum[skiftv1.Priority]{
	skiftv1.Priority_PRIORITY_P0: "p0",
	skiftv1.Priority_PRIORITY_P1: "p1",
	skiftv1.Priority_PRIORITY_P2: "p2",
	skiftv1.Priority_PRIORITY_P3: "p3",
	skiftv1.Priority_PRIORITY_P4: "p4",
}

var statuses = enum[skiftv1.Status]{
	skiftv1.Status_STATUS_OPEN:    "open",
	skiftv1.Status_STATUS_DONE:    "done",
	skiftv1.Status_STATUS_DROPPED: "dropped",
}

var assigneeKinds = enum[skiftv1.AssigneeKind]{
	skiftv1.AssigneeKind_ASSIGNEE_KIND_AGENT: domain.AssigneeAgent,
	skiftv1.AssigneeKind_ASSIGNEE_KIND_USER:  domain.AssigneeUser,
}

var principalKinds = enum[skiftv1.PrincipalKind]{
	skiftv1.PrincipalKind_PRINCIPAL_KIND_USER:    "user",
	skiftv1.PrincipalKind_PRINCIPAL_KIND_AGENT:   "agent",
	skiftv1.PrincipalKind_PRINCIPAL_KIND_SERVICE: "service",
}

var usageKinds = enum[skiftv1.UsageKind]{
	skiftv1.UsageKind_USAGE_KIND_ACTUAL:   domain.UsageActual,
	skiftv1.UsageKind_USAGE_KIND_ESTIMATE: domain.UsageEstimate,
}

var edgeKinds = enum[skiftv1.EdgeKind]{
	skiftv1.EdgeKind_EDGE_KIND_BLOCKS: "blocks",
}

var releaseReasons = enum[skiftv1.ReleaseReason]{
	skiftv1.ReleaseReason_RELEASE_REASON_COMPLETE:  domain.ReleaseComplete,
	skiftv1.ReleaseReason_RELEASE_REASON_GATE:      domain.ReleaseGate,
	skiftv1.ReleaseReason_RELEASE_REASON_CRASH:     domain.ReleaseCrash,
	skiftv1.ReleaseReason_RELEASE_REASON_BUDGET:    domain.ReleaseBudget,
	skiftv1.ReleaseReason_RELEASE_REASON_EXPIRED:   domain.ReleaseExpired,
	skiftv1.ReleaseReason_RELEASE_REASON_PREEMPTED: domain.ReleasePreempted,
}

var dropReasons = enum[skiftv1.DropReason]{
	skiftv1.DropReason_DROP_REASON_SUPERSEDED:         domain.DropSuperseded,
	skiftv1.DropReason_DROP_REASON_NOT_VIABLE:         domain.DropNotViable,
	skiftv1.DropReason_DROP_REASON_NO_LONGER_RELEVANT: domain.DropNoLongerRelevant,
	skiftv1.DropReason_DROP_REASON_OTHER:              domain.DropOther,
}

// rejectStatusHeld refuses filter.status and filter.held on a claim or a
// ready read: neither notion makes sense mid-claim, and status and held
// are ListNodes-only fields of [skiftv1.NodeFilter].
func rejectStatusHeld(f *skiftv1.NodeFilter) error {
	if f.GetStatus() != skiftv1.Status_STATUS_UNSPECIFIED || f.GetHeld() {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("status and held apply to list only, not a claim or a ready read"))
	}
	return nil
}

// enum is a two-way table between a wire enum and its stored string.
type enum[E ~int32] map[E]string

// str is the stored string for e; empty for the unspecified value or one
// the table does not know.
func (t enum[E]) str(e E) string { return t[e] }

// val is the wire value for s; the zero value for empty or unknown.
func (t enum[E]) val(s string) E {
	for e, name := range t {
		if name == s {
			return e
		}
	}
	var zero E
	return zero
}

// valPtr is val over a nullable column.
func (t enum[E]) valPtr(s *string) E {
	if s == nil {
		var zero E
		return zero
	}
	return t.val(*s)
}

// Page tokens carry an offset into the ordered set, opaque to the client.

func encodePageToken(offset int) string {
	if offset <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte("o=" + strconv.Itoa(offset)))
}

func decodePageToken(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("malformed page token %q", token))
	}
	offset, err := strconv.Atoi(strings.TrimPrefix(string(raw), "o="))
	if err != nil || !strings.HasPrefix(string(raw), "o=") || offset < 0 {
		return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("malformed page token %q", token))
	}
	return offset, nil
}

// pageSize is the page a request asked for, or the default.
func pageSize(requested int32) int {
	if requested <= 0 {
		return defaultPageSize
	}
	return int(requested)
}

// nextPageToken is the token for the page after one of got rows when the
// page was full, and empty when it was the last.
func nextPageToken(offset, size, got int) string {
	if got < size {
		return ""
	}
	return encodePageToken(offset + got)
}

// Requests to domain inputs.

func fromPatch(req *skiftv1.ApplyPatchRequest) domain.Patch {
	p := domain.Patch{RequestID: req.GetRequestId(), Project: req.GetProject(), LeaseID: req.GetLeaseId()}
	for _, op := range req.GetOps() {
		p.Ops = append(p.Ops, fromOp(op))
	}
	return p
}

func fromOp(op *skiftv1.PatchOp) domain.Op {
	switch o := op.GetOp().(type) {
	case *skiftv1.PatchOp_CreateNode:
		c := o.CreateNode
		return domain.Op{CreateNode: &domain.CreateNode{
			Alias: c.GetAlias(), Parent: c.GetParent(), BlockedBy: c.GetBlockedBy(),
			Kind: c.GetKind(), Title: c.GetTitle(), Body: c.GetBody(),
			Priority: priorities.str(c.GetPriority()), AssigneeKind: assigneeKinds.str(c.GetAssigneeKind()),
			Assignee: c.GetAssignee(), Labels: c.GetLabels(),
		}}
	case *skiftv1.PatchOp_UpdateNode:
		u := o.UpdateNode
		up := &domain.UpdateNode{Node: u.GetNode(), Parent: u.Parent, Kind: u.Kind, Title: u.Title, Body: u.Body, Assignee: u.Assignee}
		if u.Priority != nil {
			up.Priority = ptr(priorities.str(u.GetPriority()))
		}
		if u.AssigneeKind != nil {
			up.AssigneeKind = ptr(assigneeKinds.str(u.GetAssigneeKind()))
		}
		if u.Labels != nil {
			labels := u.GetLabels().GetValues()
			if labels == nil {
				labels = []string{}
			}
			up.Labels = &labels
		}
		return domain.Op{UpdateNode: up}
	case *skiftv1.PatchOp_DropNode:
		d := o.DropNode
		return domain.Op{DropNode: &domain.DropNode{Node: d.GetNode(), Reason: dropReasons.str(d.GetReason()), Detail: d.GetDetail(), Cascade: fromCascade(d.GetCascade())}}
	case *skiftv1.PatchOp_CompleteNode:
		c := o.CompleteNode
		return domain.Op{CompleteNode: &domain.CompleteNode{Node: c.GetNode(), Result: fromResult(c.GetResult())}}
	case *skiftv1.PatchOp_ReopenNode:
		r := o.ReopenNode
		return domain.Op{ReopenNode: &domain.ReopenNode{Node: r.GetNode(), Reason: r.GetReason()}}
	case *skiftv1.PatchOp_AddEdge:
		e := o.AddEdge
		return domain.Op{AddEdge: &domain.AddEdge{From: e.GetFrom(), To: e.GetTo(), Kind: edgeKinds.str(e.GetKind())}}
	case *skiftv1.PatchOp_RemoveEdge:
		e := o.RemoveEdge
		return domain.Op{RemoveEdge: &domain.RemoveEdge{From: e.GetFrom(), To: e.GetTo(), Kind: edgeKinds.str(e.GetKind())}}
	case *skiftv1.PatchOp_AttachArtifact:
		a := o.AttachArtifact
		return domain.Op{AttachArtifact: &domain.AttachArtifact{Node: a.GetNode(), Artifact: fromArtifact(a.GetArtifact())}}
	case *skiftv1.PatchOp_AddComment:
		c := o.AddComment
		return domain.Op{AddComment: &domain.AddComment{Node: c.GetNode(), Body: c.GetBody(), InReplyTo: c.GetInReplyTo()}}
	case *skiftv1.PatchOp_EditComment:
		c := o.EditComment
		return domain.Op{EditComment: &domain.EditComment{Comment: c.GetCommentId(), Body: c.GetBody()}}
	case *skiftv1.PatchOp_ResolveComment:
		c := o.ResolveComment
		return domain.Op{ResolveComment: &domain.ResolveComment{Comment: c.GetCommentId()}}
	case *skiftv1.PatchOp_EstimateNode:
		e := o.EstimateNode
		return domain.Op{EstimateNode: &domain.EstimateNode{
			Node: e.GetNode(), Model: e.GetModel(), Basis: e.GetBasis(),
			Tokens: domain.Tokens{
				InputTokens: e.GetInputTokens(), OutputTokens: e.GetOutputTokens(),
				CacheReadTokens: e.GetCacheReadTokens(), CacheWriteTokens: e.GetCacheWriteTokens(),
			},
		}}
	case *skiftv1.PatchOp_HoldNode:
		h := o.HoldNode
		return domain.Op{HoldNode: &domain.HoldNode{Node: h.GetNode(), Reason: h.GetReason()}}
	case *skiftv1.PatchOp_UnholdNode:
		u := o.UnholdNode
		return domain.Op{UnholdNode: &domain.UnholdNode{Node: u.GetNode(), Reason: u.GetReason()}}
	}
	// An empty operation reaches the domain as one, which rejects it by
	// index.
	return domain.Op{}
}

// fromCascade carries the bound a cascading drop stated. Absent on the
// wire means the drop takes only the node it names.
func fromCascade(c *skiftv1.Cascade) *domain.Cascade {
	if c == nil {
		return nil
	}
	return &domain.Cascade{MaxNodes: int(c.GetMaxNodes())}
}

func fromResult(r *skiftv1.Result) domain.NodeResult {
	out := domain.NodeResult{Body: r.GetBody()}
	for _, a := range r.GetArtifacts() {
		out.Artifacts = append(out.Artifacts, fromArtifact(a))
	}
	return out
}

// fromArtifact copies what a client may say about an artifact. The digest
// and size of a text part are computed by the server and not read here.
func fromArtifact(a *skiftv1.Artifact) domain.Artifact {
	out := domain.Artifact{Name: a.GetName(), Description: a.GetDescription()}
	if a.GetMetadata() != nil {
		out.Metadata = a.GetMetadata().AsMap()
	}
	for _, p := range a.GetParts() {
		var part domain.Part
		switch c := p.GetContent().(type) {
		case *skiftv1.Part_Text:
			part.Text = &domain.TextPart{Text: c.Text.GetText(), MediaType: c.Text.GetMediaType()}
		case *skiftv1.Part_Blob:
			part.Blob = &domain.BlobPart{Digest: c.Blob.GetDigest()}
		case *skiftv1.Part_Reference:
			part.Reference = &domain.ReferencePart{URI: c.Reference.GetUri(), Digest: c.Reference.GetDigest(), MediaType: c.Reference.GetMediaType(), Size: c.Reference.GetSize()}
		}
		out.Parts = append(out.Parts, part)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Domain results to responses.

func toNode(n domain.NodeState) *skiftv1.Node {
	out := &skiftv1.Node{
		Id:           n.ID,
		Number:       n.Number,
		OrgId:        n.OrgID,
		ProjectId:    n.ProjectID,
		ParentId:     deref(n.ParentID),
		Kind:         n.Kind,
		Title:        n.Title,
		Body:         n.Body,
		Status:       statuses.val(n.Status),
		Priority:     priorities.val(n.Priority),
		AssigneeKind: assigneeKinds.valPtr(n.AssigneeKind),
		Assignee:     deref(n.Assignee),
		Labels:       n.Labels,
		CreatedAt:    timestamppb.New(n.CreatedAt),
		Blocked:      n.Blocked,
		Ready:        n.Ready,
		Claimed:      n.Claimed,
		Held:         n.Held,
		HumanTouched: n.HumanTouched,
		Reference:    n.Ref,
		Hold:         toHold(n.HeldAt, n.HeldBy, n.HeldReason, n.HoldGroup),
	}
	if n.ContextID.Valid {
		out.ContextId = n.ContextID.String()
	}
	return out
}

// toNodes renders states in view, in order.
func toNodes(states []domain.NodeState, view skiftv1.View) []*skiftv1.Node {
	out := make([]*skiftv1.Node, 0, len(states))
	for _, s := range states {
		out = append(out, projectNode(toNode(s), view))
	}
	return out
}

func toProject(p domain.Project) *skiftv1.Project {
	out := &skiftv1.Project{
		Id:          p.ID,
		OrgId:       p.OrgID,
		Slug:        p.Slug,
		Title:       p.Title,
		Priority:    priorities.val(p.Priority),
		NodeCounter: p.NodeCounter,
		CreatedAt:   timestamppb.New(p.CreatedAt),
		VcsUrl:      p.VcsUrl,
	}
	if p.ArchivedAt != nil {
		out.ArchivedAt = timestamppb.New(*p.ArchivedAt)
	}
	out.Hold = toHold(p.HeldAt, p.HeldBy, p.HeldReason, p.HoldGroup)
	return out
}

// toHold renders the hold a node or project carries, or nil when it
// carries none.
func toHold(at *time.Time, by, reason, group *string) *skiftv1.Hold {
	if at == nil {
		return nil
	}
	return &skiftv1.Hold{
		At:     timestamppb.New(*at),
		By:     deref(by),
		Reason: deref(reason),
		Group:  deref(group),
	}
}

func toLease(l domain.Lease) *skiftv1.Lease {
	return &skiftv1.Lease{
		Id:            l.ID,
		NodeId:        l.NodeID,
		Holder:        l.Holder,
		OnBehalfOf:    deref(l.OnBehalfOf),
		Harness:       l.Harness,
		AgentRef:      l.AgentRef,
		SessionRef:    deref(l.SessionRef),
		ExpiresAt:     timestamppb.New(l.ExpiresAt),
		ReleaseReason: releaseReasons.valPtr(l.ReleaseReason),
	}
}

func toEdge(e domain.Edge) *skiftv1.Edge {
	return &skiftv1.Edge{From: e.FromNode, To: e.ToNode, Kind: edgeKinds.val(e.Kind)}
}

func toEvent(e domain.Event) (*skiftv1.Event, error) {
	out := &skiftv1.Event{
		Id:             e.ID,
		OrgId:          e.OrgID,
		ProjectId:      deref(e.ProjectID),
		NodeId:         deref(e.NodeID),
		Principal:      e.PrincipalID,
		PrincipalKind:  principalKinds.val(e.PrincipalKind),
		OnBehalfOf:     deref(e.OnBehalfOf),
		IdentityScheme: e.IdentityScheme,
		RequestId:      deref(e.RequestID),
		Op:             e.Op,
		Reason:         deref(e.Reason),
		OccurredAt:     timestamppb.New(e.OccurredAt),
	}
	var err error
	if out.Before, err = toStruct(e.Before); err != nil {
		return nil, fmt.Errorf("event %s before: %w", e.ID, err)
	}
	if out.After, err = toStruct(e.After); err != nil {
		return nil, fmt.Errorf("event %s after: %w", e.ID, err)
	}
	return out, nil
}

func toEvents(events []domain.Event) ([]*skiftv1.Event, error) {
	out := make([]*skiftv1.Event, 0, len(events))
	for _, e := range events {
		ev, err := toEvent(e)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// toStruct decodes a stored JSON object. Empty or null is no struct.
func toStruct(raw []byte) (*structpb.Struct, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	s := &structpb.Struct{}
	if err := s.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return s, nil
}

// toInputsHint renders the two integers a response carries beside a node
// so a caller knows its inputs exist and how much reading them would
// cost.
func toInputsHint(h domain.InputsHint) *skiftv1.InputsHint {
	return &skiftv1.InputsHint{Count: int32(h.Count), Bytes: h.Bytes} //nolint:gosec // a node's predecessor count is bounded by the graph
}

// toCommentsHint renders what a node's unresolved comments come to so a
// caller knows someone left a note without being handed it.
func toCommentsHint(h domain.CommentsHint) *skiftv1.CommentsHint {
	out := &skiftv1.CommentsHint{Unresolved: int32(h.Unresolved)} //nolint:gosec // a node's comment count is bounded by the comments table
	if h.LatestAt != nil {
		out.LatestAt = timestamppb.New(*h.LatestAt)
	}
	return out
}

// resultBytes is the size of a result body, and zero for a predecessor
// that has none.
func resultBytes(r *domain.StoredResult) int64 {
	if r == nil {
		return 0
	}
	return int64(len(r.Body))
}

func toResult(r *domain.StoredResult, artifacts []domain.StoredArtifact) (*skiftv1.Result, error) {
	if r == nil {
		return nil, nil
	}
	out := &skiftv1.Result{Body: r.Body}
	var err error
	if out.Artifacts, err = toArtifacts(artifacts); err != nil {
		return nil, err
	}
	return out, nil
}

func toArtifacts(artifacts []domain.StoredArtifact) ([]*skiftv1.Artifact, error) {
	out := make([]*skiftv1.Artifact, 0, len(artifacts))
	for _, a := range artifacts {
		wire, err := toArtifact(a)
		if err != nil {
			return nil, err
		}
		out = append(out, wire)
	}
	return out, nil
}

func toArtifact(a domain.StoredArtifact) (*skiftv1.Artifact, error) {
	out := &skiftv1.Artifact{Name: a.Name, Description: a.Description}
	var err error
	if out.Metadata, err = toStruct(a.Metadata); err != nil {
		return nil, fmt.Errorf("artifact %s metadata: %w", a.ID, err)
	}
	for _, p := range a.Parts {
		out.Parts = append(out.Parts, toPart(p))
	}
	return out, nil
}

// toPart renders one stored part. A text part's digest and size are the
// server's: computed from the stored text, never taken from a client.
func toPart(p domain.StoredPart) *skiftv1.Part {
	var size int64
	if p.Size != nil {
		size = *p.Size
	}
	switch p.Kind {
	case domain.PartText:
		text := deref(p.Text)
		return &skiftv1.Part{Content: &skiftv1.Part_Text{Text: &skiftv1.TextPart{
			Text: text, Digest: textDigest(text), MediaType: deref(p.MediaType), Size: size,
		}}}
	case domain.PartBlob:
		return &skiftv1.Part{Content: &skiftv1.Part_Blob{Blob: &skiftv1.BlobPart{
			Digest: deref(p.BlobDigest), MediaType: deref(p.MediaType), Size: size,
		}}}
	default:
		return &skiftv1.Part{Content: &skiftv1.Part_Reference{Reference: &skiftv1.ReferencePart{
			Uri: deref(p.ReferenceUri), Digest: deref(p.ReferenceDigest), MediaType: deref(p.MediaType), Size: size,
		}}}
	}
}

// textDigest is the canonical `sha256:` digest of inline text.
func textDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// toUsage renders one stored usage record. A record naming no model, or
// an actual, which has no basis, carries the empty string rather than a
// null: the wire has no optional here to distinguish them.
func toUsage(u domain.Usage) *skiftv1.Usage {
	return &skiftv1.Usage{
		NodeId:           u.NodeID,
		LeaseId:          deref(u.LeaseID),
		Principal:        u.Principal,
		Model:            deref(u.Model),
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		Kind:             usageKinds.val(u.Kind),
		Basis:            deref(u.Basis),
		CreatedAt:        timestamppb.New(u.CreatedAt),
	}
}

// toRollup renders what a subtree cost and what it is expected to cost.
// Nil is no rollup rather than an empty one, so a narrower view carries
// nothing.
func toRollup(r *domain.Rollup) *skiftv1.Rollup {
	if r == nil {
		return nil
	}
	return &skiftv1.Rollup{Actual: toModelTotals(r.Actual), Estimate: toModelTotals(r.Estimate)}
}

func toModelTotals(totals []domain.ModelTotals) []*skiftv1.ModelTotals {
	out := make([]*skiftv1.ModelTotals, 0, len(totals))
	for _, t := range totals {
		out = append(out, &skiftv1.ModelTotals{
			Model:            deref(t.Model),
			Records:          t.Records,
			InputTokens:      t.InputTokens,
			OutputTokens:     t.OutputTokens,
			CacheReadTokens:  t.CacheReadTokens,
			CacheWriteTokens: t.CacheWriteTokens,
		})
	}
	return out
}

func toComment(c domain.CommentHistory) *skiftv1.Comment {
	out := &skiftv1.Comment{
		Id:         c.ID,
		NodeId:     c.NodeID,
		Author:     c.Author,
		Body:       c.Body,
		InReplyTo:  deref(c.InReplyTo),
		ResolvedBy: deref(c.ResolvedBy),
	}
	if c.ResolvedAt != nil {
		out.ResolvedAt = timestamppb.New(*c.ResolvedAt)
	}
	for _, r := range c.Revisions {
		out.Revisions = append(out.Revisions, &skiftv1.CommentRevision{Body: r.Body, Author: r.Author, CreatedAt: timestamppb.New(r.CreatedAt)})
	}
	return out
}

// toReason renders one readiness condition. Nodes the reason names carry
// the derived state the explanation computed for them.
func toReason(r domain.Reason, derived map[string]domain.Derived) *skiftv1.Reason {
	switch r.Kind {
	case domain.ReasonNotOpen:
		return &skiftv1.Reason{Reason: &skiftv1.Reason_NotOpen{NotOpen: &skiftv1.NotOpenReason{Status: statuses.val(r.Status)}}}
	case domain.ReasonBlocked:
		blocker := toNode(domain.NodeState{Node: *r.Blocker, Derived: derived[r.Blocker.ID]})
		return &skiftv1.Reason{Reason: &skiftv1.Reason_Blocked{Blocked: &skiftv1.BlockedReason{Blocker: blocker, Gate: r.Gate}}}
	case domain.ReasonLeased:
		return &skiftv1.Reason{Reason: &skiftv1.Reason_Leased{Leased: &skiftv1.LeasedReason{Lease: toLease(*r.Lease)}}}
	case domain.ReasonOpenChild:
		child := toNode(domain.NodeState{Node: *r.Child, Derived: derived[r.Child.ID]})
		return &skiftv1.Reason{Reason: &skiftv1.Reason_OpenChild{OpenChild: &skiftv1.OpenChildReason{Child: child}}}
	case domain.ReasonArchived:
		return &skiftv1.Reason{Reason: &skiftv1.Reason_Archived{Archived: &skiftv1.ArchivedReason{Project: toProject(*r.Project)}}}
	case domain.ReasonProjectHeld:
		return &skiftv1.Reason{Reason: &skiftv1.Reason_ProjectHeld{ProjectHeld: &skiftv1.ProjectHeldReason{Project: toProject(*r.Project)}}}
	case domain.ReasonHeld:
		h := r.Hold
		return &skiftv1.Reason{Reason: &skiftv1.Reason_Held{Held: &skiftv1.HeldReason{
			Number: h.Number, Title: h.Title, Self: h.Self,
			Hold: toHold(&h.At, &h.By, &h.Reason, h.Group),
		}}}
	}
	return &skiftv1.Reason{}
}

func toHit(h domain.Hit, view skiftv1.View) *skiftv1.SearchHit {
	// A search hit is a node without derived state: the page is ranked
	// text, and readiness is Explain's to answer.
	node := toNode(domain.NodeState{Node: h.Node, Derived: domain.Derived{Ref: h.Ref}})
	return &skiftv1.SearchHit{Node: projectNode(node, view), Snippet: h.Snippet}
}

// fromDuration reads an optional wire duration. Unset is zero, which the
// domain reads as "the default".
func fromDuration(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return 0, nil
	}
	if err := d.CheckValid(); err != nil {
		return 0, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if d.AsDuration() < 0 {
		return 0, connect.NewError(connect.CodeInvalidArgument, errors.New("duration is negative"))
	}
	return d.AsDuration(), nil
}
