// Package blob holds the bytes of managed blobs. The store records a
// blob's identity, media type, and size in a row scoped to its
// organization; a [Backend] holds the bytes under their digest and
// nothing else. Filesystem first, object storage later, behind the same
// interface. See docs/adrs/003-artifacts-are-content-addressed.md.
//
// A backend is content-addressed and immutable: the only way to put bytes
// in is an [Upload] that seals them under their SHA-256 on finalize, and
// identical bytes finalized twice are held once. It knows nothing of
// organizations; tenancy is the blobs table's, which is why the domain
// reads a blob's row before opening its bytes.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"time"
)

// Errors a backend reports.
var (
	// ErrNotFound: no finalized blob has that digest.
	ErrNotFound = errors.New("blob not found")
	// ErrFinalized: the upload is sealed, and takes no more bytes.
	ErrFinalized = errors.New("upload is already finalized")
	// ErrAborted: the upload was abandoned or reclaimed, so there is
	// nothing to finalize.
	ErrAborted = errors.New("upload was aborted")
	// ErrDigest: a digest is not in the canonical sha256:<hex> form.
	ErrDigest = errors.New("digest is not sha256:<hex>")
)

// Backend holds blob bytes by digest.
type Backend interface {
	// Name is what blobs.backend records, so a row says where its bytes
	// are.
	Name() string
	// Begin starts an upload. Nothing is addressable until the upload is
	// finalized; an upload that is abandoned is reclaimed by [Backend.Reclaim].
	Begin(ctx context.Context) (Upload, error)
	// Open reads the bytes finalized under digest, or [ErrNotFound].
	Open(ctx context.Context, digest string) (io.ReadCloser, error)
	// Stat reports the size of the bytes finalized under digest, or
	// [ErrNotFound].
	Stat(ctx context.Context, digest string) (int64, error)
	// Reclaim removes every upload begun before cutoff and never
	// finalized, and reports how many. An upload still in flight past the
	// cutoff is reclaimed too, and fails when it is finalized.
	Reclaim(ctx context.Context, cutoff time.Time) (int, error)
}

// Upload receives the bytes of one blob. Write it, then Finalize it; or
// Abort it, and nothing of it remains. The backend computes the digest,
// so a caller cannot file bytes under a name they do not hash to.
type Upload interface {
	io.Writer
	// Finalize seals the bytes written so far under their digest and
	// returns it. Bytes already held under that digest are kept and the
	// upload's copy discarded, so identical bytes are stored once.
	Finalize(ctx context.Context) (Sealed, error)
	// Abort discards the upload. It is a no-op after Finalize, and safe
	// to call twice.
	Abort() error
}

// Sealed is what finalizing an upload reports.
type Sealed struct {
	// Digest is the canonical sha256:<hex> of the bytes.
	Digest string
	// Size is the byte count.
	Size int64
	// Existed reports that the backend already held these bytes, and
	// this upload added nothing.
	Existed bool
}

// digestShape is the canonical digest form the schema checks.
var digestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidDigest reports whether digest is in the canonical sha256:<hex>
// form.
func ValidDigest(digest string) bool {
	return digestShape.MatchString(digest)
}

// Digest is the canonical digest of a SHA-256 sum.
func Digest(sum [sha256.Size]byte) string {
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DigestOf is the canonical digest of bytes, for callers verifying what
// they pulled against what a part records.
func DigestOf(b []byte) string {
	return Digest(sha256.Sum256(b))
}
