package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/store/blob"
)

// An artifact is a named output attached to a node, shaped after A2A:
// name, description, parts, metadata. A part is inline text under the
// cap, a managed blob, or a reference to something that lives elsewhere.
// Dagger is the manifest, not the pipe: it never stores raw bytes inside
// a node, and it never fetches a reference. See the Artifact section of
// docs/domain-model.md and docs/adrs/003-artifacts-are-content-addressed.md.
//
// A managed blob is uploaded before it is attached. [UploadBlob] streams
// the bytes into a [blob.Backend], which seals them under their SHA-256,
// and only then records the blob's row; an attach names a digest and is
// refused unless that row exists. A partial upload therefore has nothing
// to attach, and identical bytes uploaded twice are one row and one
// object.

// Errors an artifact operation is rejected with. An inline part or a blob
// over its ceiling is a [*LimitError]; see limits.go.
var (
	// ErrBlobNotFound: a blob part names a digest the organization holds
	// no blob for.
	ErrBlobNotFound = errors.New("no managed blob with that digest")
	// ErrDigestMismatch: the bytes uploaded do not hash to the digest the
	// caller said they would.
	ErrDigestMismatch = errors.New("uploaded bytes do not match the expected digest")
	// ErrBlobElsewhere: the blob's row names a backend other than the one
	// asked to open it.
	ErrBlobElsewhere = errors.New("blob is held by another backend")
	// ErrGitCommitRequired: a git reference records the commit it
	// resolved to, not only the branch or pull request it names.
	ErrGitCommitRequired = errors.New("a git reference records its resolved commit as a rev query parameter")
	// ErrArtifactNameTaken: the node already carries an artifact by that
	// name.
	ErrArtifactNameTaken = errors.New("the node already carries an artifact by that name")
)

// OpAttachArtifact is the operation name, as written to events.op.
const OpAttachArtifact = "attach_artifact"

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
//
// The scheme is the contract. A git reference (scheme git, or git+<transport>)
// names the branch, tag, or pull request it points at however it likes,
// and records the commit that resolved to as a full hex hash in a `rev`
// query parameter. An oci reference names a registry blob by digest as
// the registry does. Anything else is passed through as the client gave
// it.
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

// defaultBlobMediaType is what an upload carries when it names none.
const defaultBlobMediaType = "application/octet-stream"

// digestShape is the canonical digest form the schema checks.
var digestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// gitCommitShape is a full commit hash: SHA-1, or SHA-256 in a repository
// using that object format. An abbreviated hash is not an immutable
// identity, since it can become ambiguous as the repository grows.
var gitCommitShape = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// AttachArtifact attaches an artifact to a node outside a completion, as
// a completion does with its result's artifacts. A held node is fenced
// like any other write.
type AttachArtifact struct {
	// Node is a node reference.
	Node string `json:"node"`
	// Artifact is what to attach. Its name must be unique within the
	// node.
	Artifact Artifact `json:"artifact"`
}

// validate rejects an attach that cannot be applied however the graph
// stands.
func (op *AttachArtifact) validate(l Limits) error {
	return op.Artifact.validate(l)
}

// validate rejects an artifact with no name or no parts, or a part not of
// exactly one kind or over its ceiling.
func (a *Artifact) validate(l Limits) error {
	if a.Name == "" {
		return errors.New("a name is required")
	}
	if len(a.Parts) == 0 {
		return errors.New("at least one part is required")
	}
	for j := range a.Parts {
		if err := a.Parts[j].validate(l); err != nil {
			return fmt.Errorf("part %d: %w", j, err)
		}
	}
	return nil
}

func (p *Part) validate(l Limits) error {
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
		if err := exceeded(LimitInlinePartBytes, int64(len(p.Text.Text)), int64(l.InlinePartBytes)); err != nil {
			return err
		}
	case p.Blob != nil:
		if !digestShape.MatchString(p.Blob.Digest) {
			return fmt.Errorf("blob digest %q is not sha256:<hex>", p.Blob.Digest)
		}
	case p.Reference != nil:
		return p.Reference.validate()
	}
	return nil
}

// validate rejects a reference with no URI, a digest not in canonical
// form, or a git URI that does not record the commit it resolved to. The
// URI is otherwise taken as given: dagger stores it verbatim and never
// resolves it.
func (r *ReferencePart) validate() error {
	if r.URI == "" {
		return errors.New("a reference needs a uri")
	}
	if !digestShape.MatchString(r.Digest) {
		return fmt.Errorf("reference digest %q is not sha256:<hex>", r.Digest)
	}
	if r.Size < 0 {
		return errors.New("reference size is negative")
	}
	if isGitURI(r.URI) {
		u, err := url.Parse(r.URI)
		if err != nil {
			return fmt.Errorf("git reference %q: %w", r.URI, err)
		}
		if !gitCommitShape.MatchString(u.Query().Get("rev")) {
			return fmt.Errorf("git reference %q: %w", r.URI, ErrGitCommitRequired)
		}
	}
	return nil
}

// isGitURI reports whether a URI's scheme is git or git+<transport>,
// compared case-insensitively as schemes are.
func isGitURI(uri string) bool {
	colon := strings.IndexByte(uri, ':')
	if colon < 0 {
		return false
	}
	scheme := strings.ToLower(uri[:colon])
	return scheme == "git" || strings.HasPrefix(scheme, "git+")
}

// StoredPart is a part as a reader receives it: its row, and whether its
// content is agent-supplied input the reader must not act on unchecked.
type StoredPart struct {
	store.ArtifactPart
	// Untrusted is set on every reference part. Its URI is stored
	// verbatim as the attaching agent gave it and dagger never fetched
	// it, so whoever does is responsible for scheme and host
	// restrictions and for refusing link-local and cloud metadata
	// destinations. The digest proves the bytes fetched are the bytes
	// attached, and nothing about their provenance or safety.
	Untrusted bool `json:"untrusted,omitempty"`
}

// StoredArtifact is an artifact as the store holds it: its row and its
// parts in order.
type StoredArtifact struct {
	store.Artifact
	Parts []StoredPart `json:"parts"`
}

// Attached is what an attach_artifact event records as its after: the
// node the artifact went on, which the attach leaves as it stands, and
// the artifact itself.
type Attached struct {
	Node     store.Node     `json:"node"`
	Artifact StoredArtifact `json:"artifact"`
}

// readArtifacts returns every artifact of a node with its parts, in
// attachment order.
func readArtifacts(ctx context.Context, tx store.Tx, orgID, nodeID string) ([]StoredArtifact, error) {
	rows, err := tx.ListArtifactsForNode(ctx, store.ListArtifactsForNodeParams{OrgID: orgID, NodeID: nodeID})
	if err != nil {
		return nil, fmt.Errorf("read artifacts of node %s: %w", nodeID, err)
	}
	out := make([]StoredArtifact, 0, len(rows))
	for _, a := range rows {
		parts, err := tx.ListArtifactParts(ctx, store.ListArtifactPartsParams{OrgID: orgID, ArtifactID: a.ID})
		if err != nil {
			return nil, fmt.Errorf("read parts of artifact %s: %w", a.ID, err)
		}
		stored := StoredArtifact{Artifact: a, Parts: make([]StoredPart, 0, len(parts))}
		for _, p := range parts {
			stored.Parts = append(stored.Parts, storedPart(p))
		}
		out = append(out, stored)
	}
	return out, nil
}

// storedPart is a part row as a reader receives it.
func storedPart(p store.ArtifactPart) StoredPart {
	return StoredPart{ArtifactPart: p, Untrusted: p.Kind == PartReference}
}

func (a *applier) attachArtifact(ctx context.Context, op *AttachArtifact) error {
	resolved, err := a.resolver.Resolve(ctx, op.Node)
	if err != nil {
		return err
	}
	if err := a.fence(ctx, resolved.ID); err != nil {
		return err
	}
	node, err := a.tx.GetNode(ctx, store.GetNodeParams{OrgID: a.orgID, ID: resolved.ID})
	if err != nil {
		return fmt.Errorf("read node %s: %w", op.Node, err)
	}
	if err := a.uniqueArtifactNames(ctx, node.ID, []Artifact{op.Artifact}); err != nil {
		return err
	}
	stored, err := a.attach(ctx, node.ID, &op.Artifact)
	if err != nil {
		return err
	}
	return a.event(ctx, OpAttachArtifact, node.ID, node.ProjectID, nil, node, Attached{Node: node, Artifact: stored})
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
			return fmt.Errorf("artifact %q: %w", artifacts[i].Name, ErrArtifactNameTaken)
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
	stored := StoredArtifact{Artifact: row, Parts: make([]StoredPart, 0, len(art.Parts))}
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
		stored.Parts = append(stored.Parts, storedPart(part))
	}
	return stored, nil
}

// partParams is the row one part becomes. A blob part is resolved against
// the organization's blobs, and takes its media type and size from the
// row it names; a digest with no row is a blob that was never finalized,
// or never uploaded, and cannot be attached. A reference is stored as
// given.
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

// BlobUpload is what [UploadBlob] takes.
type BlobUpload struct {
	// OrgID is the organization the blob belongs to.
	OrgID string
	// MediaType defaults to application/octet-stream.
	MediaType string
	// Digest, when set, is what the caller says the bytes hash to. An
	// upload whose bytes hash to anything else is discarded.
	Digest string
	// Body is the bytes.
	Body io.Reader
}

// UploadBlob streams the bytes into the backend, seals them under their
// SHA-256, and records the blob for the organization. It returns the
// blob's row, which is the same row when the organization already holds
// those bytes. Nothing is recorded, and nothing stays addressable, unless
// every byte arrived and the backend finalized them: a read error, a body
// over [Limits.BlobBytes], or a digest other than the one expected leaves
// only an aborted upload behind, and that is reclaimed.
//
// The bytes travel outside any transaction, since a stream can be slow
// and a connection should not wait on it. The row is written in a
// transaction of its own once the bytes are sealed.
func UploadBlob(ctx context.Context, s *store.Store, backend blob.Backend, in BlobUpload) (store.Blob, error) {
	if in.OrgID == "" {
		return store.Blob{}, errors.New("an organization is required")
	}
	if in.Digest != "" && !digestShape.MatchString(in.Digest) {
		return store.Blob{}, fmt.Errorf("expected digest %q is not sha256:<hex>", in.Digest)
	}
	if in.Body == nil {
		return store.Blob{}, errors.New("a body is required")
	}
	mediaType := in.MediaType
	if mediaType == "" {
		mediaType = defaultBlobMediaType
	}
	limit := ActiveLimits().BlobBytes

	upload, err := backend.Begin(ctx)
	if err != nil {
		return store.Blob{}, fmt.Errorf("begin upload: %w", err)
	}
	// One byte past the ceiling tells an over-sized body from one that is
	// exactly at it, without reading the rest.
	n, err := io.Copy(upload, io.LimitReader(in.Body, limit+1))
	if err != nil {
		return store.Blob{}, errors.Join(fmt.Errorf("upload: %w", err), upload.Abort())
	}
	if err := exceeded(LimitBlobBytes, n, limit); err != nil {
		return store.Blob{}, errors.Join(err, upload.Abort())
	}
	sealed, err := upload.Finalize(ctx)
	if err != nil {
		return store.Blob{}, errors.Join(fmt.Errorf("finalize upload: %w", err), upload.Abort())
	}
	if in.Digest != "" && sealed.Digest != in.Digest {
		// The bytes are sealed under their real digest and may be what
		// another upload holds; the backend keeps or drops them on its
		// own terms. What matters is that no row records them for this
		// caller under the digest it expected.
		return store.Blob{}, fmt.Errorf("expected %s, uploaded %s: %w", in.Digest, sealed.Digest, ErrDigestMismatch)
	}
	var row store.Blob
	err = s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		row, err = tx.CreateBlob(ctx, store.CreateBlobParams{
			OrgID: in.OrgID, Digest: sealed.Digest, MediaType: mediaType, Size: sealed.Size, Backend: backend.Name(),
		})
		return err
	})
	if err != nil {
		return store.Blob{}, fmt.Errorf("record blob %s: %w", sealed.Digest, err)
	}
	return row, nil
}

// BlobInfo is a blob as it is read: its row, and how many artifact parts
// in the organization name it. The count is derived rather than stored.
type BlobInfo struct {
	store.Blob
	References int64 `json:"references"`
}

// ReadBlob returns the organization's blob with that digest, with its
// reference count.
func ReadBlob(ctx context.Context, tx store.Tx, orgID, digest string) (*BlobInfo, error) {
	row, err := tx.GetBlob(ctx, store.GetBlobParams{OrgID: orgID, Digest: digest})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%s: %w", digest, ErrBlobNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", digest, err)
	}
	refs, err := tx.CountBlobReferences(ctx, store.CountBlobReferencesParams{OrgID: orgID, BlobDigest: &row.Digest})
	if err != nil {
		return nil, fmt.Errorf("count references to blob %s: %w", digest, err)
	}
	return &BlobInfo{Blob: row, References: refs}, nil
}

// OpenBlob returns the organization's blob with that digest and a reader
// over its bytes. The row is what scopes the bytes to a tenant: a digest
// the organization holds no row for is not found, whatever the backend
// holds. The caller closes the reader.
func OpenBlob(ctx context.Context, tx store.Tx, backend blob.Backend, orgID, digest string) (*BlobInfo, io.ReadCloser, error) {
	info, err := ReadBlob(ctx, tx, orgID, digest)
	if err != nil {
		return nil, nil, err
	}
	if info.Backend != backend.Name() {
		return nil, nil, fmt.Errorf("blob %s is held by %q, not %q: %w", digest, info.Backend, backend.Name(), ErrBlobElsewhere)
	}
	r, err := backend.Open(ctx, info.Digest)
	if err != nil {
		return nil, nil, fmt.Errorf("open blob %s: %w", digest, err)
	}
	return info, r, nil
}
