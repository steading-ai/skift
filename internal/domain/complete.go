package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/ivy/dagger/internal/store"
)

// Completion carries the result: a markdown body under the size cap, plus
// the artifacts the work produced. Results and artifacts of a node's
// blocking predecessors are what a downstream agent assembles its context
// from, which is what makes graph resume possible. See Completion and
// results, and Artifact, in docs/domain-model.md.

// Size ceilings on what a completion carries. See the Limits section of
// docs/domain-model.md.
const (
	// MaxResultBytes bounds a result body.
	MaxResultBytes = 256 << 10
	// MaxInlinePartBytes bounds one inline text part.
	MaxInlinePartBytes = 256 << 10
)

// Errors a complete_node is rejected with.
var (
	// ErrResultRequired: a completion carries a result body, since the
	// result is what downstream reads.
	ErrResultRequired = errors.New("a completion needs a result body")
	// ErrResultTooLarge: the body exceeds [MaxResultBytes].
	ErrResultTooLarge = errors.New("result body exceeds the size cap")
	// ErrPartTooLarge: an inline part exceeds [MaxInlinePartBytes].
	ErrPartTooLarge = errors.New("inline part exceeds the size cap")
	// ErrBlobNotFound: a blob part names a digest the organization holds
	// no blob for.
	ErrBlobNotFound = errors.New("no managed blob with that digest")
)

// CompleteNode moves an open node to done with the result it produced.
// The node must be open. A held node is fenced like any other write: the
// patch must carry the node's current lease, so a worker whose lease was
// reclaimed cannot land a late result.
type CompleteNode struct {
	// Node is a node reference.
	Node string `json:"node"`
	// Result is what the work produced.
	Result NodeResult `json:"result"`
}

// NodeResult is what completing a node produces: a markdown summary and
// zero or more artifacts.
type NodeResult struct {
	// Body is markdown under [MaxResultBytes]. Required.
	Body string `json:"body"`
	// Artifacts are outputs beyond text, attached to the node on
	// completion.
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// Artifact is a named output, shaped after A2A: name, description, parts,
// metadata. See docs/adrs/003-artifacts-are-content-addressed.md.
type Artifact struct {
	// Name is unique within its node.
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parts is the content, in order. At least one.
	Parts []Part `json:"parts"`
	// Metadata is free-form, as in A2A.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Part is one piece of an artifact. Exactly one field is set.
type Part struct {
	Text      *TextPart      `json:"text,omitempty"`
	Blob      *BlobPart      `json:"blob,omitempty"`
	Reference *ReferencePart `json:"reference,omitempty"`
}

// TextPart is inline text stored with the node. The server computes its
// size and digest.
type TextPart struct {
	Text string `json:"text"`
	// MediaType defaults to text/plain.
	MediaType string `json:"media_type,omitempty"`
}

// BlobPart names a managed blob the organization already holds, by its
// digest. Media type and size come from the blob.
type BlobPart struct {
	Digest string `json:"digest"`
}

// ReferencePart points at content dagger does not store. The URI is
// agent-supplied and untrusted; whoever fetches it applies its own
// restrictions. The digest is of the referenced content, and a reference
// to something mutable carries the resolved immutable identity.
type ReferencePart struct {
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type,omitempty"`
	// Size in bytes, when known.
	Size int64 `json:"size,omitempty"`
}

// Part kinds, as artifact_parts.kind stores them.
const (
	PartText      = "text"
	PartBlob      = "blob"
	PartReference = "reference"
)

// defaultTextMediaType is what an inline part carries when it names none.
const defaultTextMediaType = "text/plain"

// digestShape is the canonical digest form the schema checks.
var digestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// validate rejects a completion that cannot be applied however the graph
// stands: no body, a body or part over its cap, an artifact with no name
// or no parts, or a part not of exactly one kind.
func (c *CompleteNode) validate() error {
	r := c.Result
	if r.Body == "" {
		return ErrResultRequired
	}
	if len(r.Body) > MaxResultBytes {
		return fmt.Errorf("%w: %d bytes, cap %d", ErrResultTooLarge, len(r.Body), MaxResultBytes)
	}
	names := map[string]bool{}
	for i := range r.Artifacts {
		a := &r.Artifacts[i]
		if a.Name == "" {
			return fmt.Errorf("artifact %d: a name is required", i)
		}
		if names[a.Name] {
			return fmt.Errorf("artifact %q: named twice in one result", a.Name)
		}
		names[a.Name] = true
		if len(a.Parts) == 0 {
			return fmt.Errorf("artifact %q: at least one part is required", a.Name)
		}
		for j := range a.Parts {
			if err := a.Parts[j].validate(); err != nil {
				return fmt.Errorf("artifact %q part %d: %w", a.Name, j, err)
			}
		}
	}
	return nil
}

func (p *Part) validate() error {
	set := 0
	for _, present := range []bool{p.Text != nil, p.Blob != nil, p.Reference != nil} {
		if present {
			set++
		}
	}
	if set != 1 {
		return errors.New("exactly one of text, blob, or reference is set")
	}
	switch {
	case p.Text != nil:
		if len(p.Text.Text) > MaxInlinePartBytes {
			return fmt.Errorf("%w: %d bytes, cap %d", ErrPartTooLarge, len(p.Text.Text), MaxInlinePartBytes)
		}
	case p.Blob != nil:
		if !digestShape.MatchString(p.Blob.Digest) {
			return fmt.Errorf("blob digest %q is not sha256:<hex>", p.Blob.Digest)
		}
	case p.Reference != nil:
		if p.Reference.URI == "" {
			return errors.New("a reference needs a uri")
		}
		if !digestShape.MatchString(p.Reference.Digest) {
			return fmt.Errorf("reference digest %q is not sha256:<hex>", p.Reference.Digest)
		}
		if p.Reference.Size < 0 {
			return errors.New("reference size is negative")
		}
	}
	return nil
}

// StoredArtifact is an artifact as the store holds it: its row and its
// parts in order.
type StoredArtifact struct {
	store.Artifact
	Parts []store.ArtifactPart `json:"parts"`
}

// Completed is what a complete_node event records as its after: the node
// as done, the result row, and the artifacts attached with it. The result
// travels on the event so the log reconstructs what the node produced,
// not only that it did.
type Completed struct {
	Node      store.Node       `json:"node"`
	Result    store.Result     `json:"result"`
	Artifacts []StoredArtifact `json:"artifacts"`
}

func (a *applier) completeNode(ctx context.Context, op *CompleteNode) error {
	resolved, err := a.resolver.Resolve(ctx, op.Node)
	if err != nil {
		return err
	}
	if err := a.fence(ctx, resolved.ID); err != nil {
		return err
	}
	before, err := a.tx.GetNode(ctx, store.GetNodeParams{OrgID: a.orgID, ID: resolved.ID})
	if err != nil {
		return fmt.Errorf("read node %s: %w", op.Node, err)
	}
	if before.Status != "open" {
		return fmt.Errorf("node %s is %s", op.Node, before.Status)
	}
	if err := a.uniqueArtifactNames(ctx, before.ID, op.Result.Artifacts); err != nil {
		return err
	}

	after, err := a.tx.SetNodeStatus(ctx, store.SetNodeStatusParams{OrgID: a.orgID, ID: before.ID, Status: "done"})
	if err != nil {
		return fmt.Errorf("%s %s: %w", OpCompleteNode, op.Node, err)
	}
	result, err := a.tx.CreateResult(ctx, store.CreateResultParams{
		ID: newULID(), OrgID: a.orgID, NodeID: after.ID, Body: op.Result.Body, CompletedBy: a.actor.PrincipalID,
	})
	if err != nil {
		return fmt.Errorf("record result of %s: %w", op.Node, err)
	}
	artifacts := make([]StoredArtifact, 0, len(op.Result.Artifacts))
	for i := range op.Result.Artifacts {
		stored, err := a.attach(ctx, after.ID, &op.Result.Artifacts[i])
		if err != nil {
			return err
		}
		artifacts = append(artifacts, stored)
	}
	a.record(after)
	return a.event(ctx, OpCompleteNode, after.ID, after.ProjectID, nil, before, Completed{Node: after, Result: result, Artifacts: artifacts})
}

// uniqueArtifactNames rejects an artifact named like one the node already
// carries.
func (a *applier) uniqueArtifactNames(ctx context.Context, nodeID string, artifacts []Artifact) error {
	if len(artifacts) == 0 {
		return nil
	}
	existing, err := a.tx.ListArtifactsForNode(ctx, store.ListArtifactsForNodeParams{OrgID: a.orgID, NodeID: nodeID})
	if err != nil {
		return fmt.Errorf("read artifacts of node %s: %w", nodeID, err)
	}
	taken := map[string]bool{}
	for _, e := range existing {
		taken[e.Name] = true
	}
	for i := range artifacts {
		if taken[artifacts[i].Name] {
			return fmt.Errorf("artifact %q: the node already carries one by that name", artifacts[i].Name)
		}
	}
	return nil
}

// attach writes one artifact and its parts under the node, authored by
// the actor.
func (a *applier) attach(ctx context.Context, nodeID string, art *Artifact) (StoredArtifact, error) {
	metadata := art.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return StoredArtifact{}, fmt.Errorf("artifact %q: encode metadata: %w", art.Name, err)
	}
	row, err := a.tx.CreateArtifact(ctx, store.CreateArtifactParams{
		ID: newULID(), OrgID: a.orgID, NodeID: nodeID, Name: art.Name, Description: art.Description,
		Metadata: encoded, CreatedBy: a.actor.PrincipalID,
	})
	if err != nil {
		return StoredArtifact{}, fmt.Errorf("attach artifact %q: %w", art.Name, err)
	}
	stored := StoredArtifact{Artifact: row, Parts: make([]store.ArtifactPart, 0, len(art.Parts))}
	for i := range art.Parts {
		params, err := a.partParams(ctx, &art.Parts[i])
		if err != nil {
			return StoredArtifact{}, fmt.Errorf("artifact %q part %d: %w", art.Name, i, err)
		}
		params.OrgID, params.ArtifactID, params.Position = a.orgID, row.ID, int32(i) //nolint:gosec // bounded by the parts slice
		part, err := a.tx.CreateArtifactPart(ctx, params)
		if err != nil {
			return StoredArtifact{}, fmt.Errorf("artifact %q part %d: %w", art.Name, i, err)
		}
		stored.Parts = append(stored.Parts, part)
	}
	return stored, nil
}

// partParams is the row one part becomes. A blob part is resolved against
// the organization's blobs, and takes its media type and size from the
// row it names.
func (a *applier) partParams(ctx context.Context, p *Part) (store.CreateArtifactPartParams, error) {
	switch {
	case p.Text != nil:
		mediaType := p.Text.MediaType
		if mediaType == "" {
			mediaType = defaultTextMediaType
		}
		size := int64(len(p.Text.Text))
		return store.CreateArtifactPartParams{Kind: PartText, Text: &p.Text.Text, MediaType: &mediaType, Size: &size}, nil
	case p.Blob != nil:
		blob, err := a.tx.GetBlob(ctx, store.GetBlobParams{OrgID: a.orgID, Digest: p.Blob.Digest})
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CreateArtifactPartParams{}, fmt.Errorf("%s: %w", p.Blob.Digest, ErrBlobNotFound)
		}
		if err != nil {
			return store.CreateArtifactPartParams{}, fmt.Errorf("read blob %s: %w", p.Blob.Digest, err)
		}
		return store.CreateArtifactPartParams{Kind: PartBlob, BlobDigest: &blob.Digest, MediaType: &blob.MediaType, Size: &blob.Size}, nil
	case p.Reference != nil:
		params := store.CreateArtifactPartParams{
			Kind: PartReference, ReferenceUri: &p.Reference.URI, ReferenceDigest: &p.Reference.Digest,
			MediaType: optional(p.Reference.MediaType),
		}
		if p.Reference.Size > 0 {
			params.Size = &p.Reference.Size
		}
		return params, nil
	}
	return store.CreateArtifactPartParams{}, errors.New("no part content set")
}
