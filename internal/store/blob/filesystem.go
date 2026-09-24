package blob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FilesystemName is what a [Filesystem] backend records in blobs.backend.
const FilesystemName = "fs"

// Filesystem holds blobs as files under a root directory.
//
// A finalized blob lives at blobs/sha256/<hex[:2]>/<hex>, read-only. An
// upload in flight is a file under uploads/ with a random name, and moves
// into place only when it is finalized; a crash leaves it under uploads/
// for [Filesystem.Reclaim]. The move is a hard link that fails when the
// target exists, so two uploads of the same bytes racing to finalize
// both succeed and the bytes are held once.
type Filesystem struct {
	root string
}

// NewFilesystem opens, creating if needed, a backend rooted at root.
func NewFilesystem(root string) (*Filesystem, error) {
	for _, dir := range []string{filepath.Join(root, "blobs", "sha256"), filepath.Join(root, "uploads")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create blob directory: %w", err)
		}
	}
	return &Filesystem{root: root}, nil
}

// Name implements [Backend].
func (f *Filesystem) Name() string { return FilesystemName }

// path is where the bytes of digest live, or [ErrDigest] when digest is
// not canonical and so could name a path outside the blob tree.
func (f *Filesystem) path(digest string) (string, error) {
	if !ValidDigest(digest) {
		return "", fmt.Errorf("%q: %w", digest, ErrDigest)
	}
	hex := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(f.root, "blobs", "sha256", hex[:2], hex), nil
}

// Open implements [Backend].
func (f *Filesystem) Open(_ context.Context, digest string) (io.ReadCloser, error) {
	p, err := f.path(digest)
	if err != nil {
		return nil, err
	}
	r, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", digest, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("open blob %s: %w", digest, err)
	}
	return r, nil
}

// Stat implements [Backend].
func (f *Filesystem) Stat(_ context.Context, digest string) (int64, error) {
	p, err := f.path(digest)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("%s: %w", digest, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("stat blob %s: %w", digest, err)
	}
	return info.Size(), nil
}

// Begin implements [Backend].
func (f *Filesystem) Begin(_ context.Context) (Upload, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("name upload: %w", err)
	}
	p := filepath.Join(f.root, "uploads", hex.EncodeToString(nonce[:]))
	file, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("begin upload: %w", err)
	}
	return &fsUpload{fs: f, path: p, file: file, hash: sha256.New()}, nil
}

// Reclaim implements [Backend].
func (f *Filesystem) Reclaim(ctx context.Context, cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(filepath.Join(f.root, "uploads"))
	if err != nil {
		return 0, fmt.Errorf("list uploads: %w", err)
	}
	reclaimed := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return reclaimed, err
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			// Finalized or aborted since the listing.
			continue
		}
		if err != nil {
			return reclaimed, fmt.Errorf("stat upload %s: %w", e.Name(), err)
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		err = os.Remove(filepath.Join(f.root, "uploads", e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return reclaimed, fmt.Errorf("reclaim upload %s: %w", e.Name(), err)
		}
		reclaimed++
	}
	return reclaimed, nil
}

// fsUpload is one upload in flight against a [Filesystem].
type fsUpload struct {
	fs   *Filesystem
	path string
	file *os.File
	hash hash.Hash
	size int64
	done bool
}

func (u *fsUpload) Write(p []byte) (int, error) {
	if u.done {
		return 0, ErrFinalized
	}
	if u.file == nil {
		return 0, ErrAborted
	}
	n, err := u.file.Write(p)
	u.hash.Write(p[:n])
	u.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("write upload: %w", err)
	}
	return n, nil
}

func (u *fsUpload) Finalize(_ context.Context) (Sealed, error) {
	if u.done {
		return Sealed{}, ErrFinalized
	}
	if u.file == nil {
		return Sealed{}, ErrAborted
	}
	u.done = true
	file := u.file
	u.file = nil
	// Whatever happens below, the upload's own file does not outlive it.
	defer os.Remove(u.path) //nolint:errcheck // best effort; Reclaim sweeps what remains
	if err := file.Sync(); err != nil {
		file.Close() //nolint:errcheck,gosec // the sync error is the one to report
		return Sealed{}, fmt.Errorf("sync upload: %w", err)
	}
	if err := file.Close(); err != nil {
		return Sealed{}, fmt.Errorf("close upload: %w", err)
	}
	var sum [sha256.Size]byte
	u.hash.Sum(sum[:0])
	sealed := Sealed{Digest: Digest(sum), Size: u.size}
	final, err := u.fs.path(sealed.Digest)
	if err != nil {
		return Sealed{}, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return Sealed{}, fmt.Errorf("create blob directory: %w", err)
	}
	// Read-only before it is addressable, so nothing ever sees a
	// writable blob.
	if err := os.Chmod(u.path, 0o444); err != nil {
		return Sealed{}, fmt.Errorf("seal upload: %w", err)
	}
	switch err := os.Link(u.path, final); {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		sealed.Existed = true
	default:
		return Sealed{}, fmt.Errorf("place blob %s: %w", sealed.Digest, err)
	}
	return sealed, nil
}

func (u *fsUpload) Abort() error {
	if u.done || u.file == nil {
		return nil
	}
	file := u.file
	u.file = nil
	err := file.Close()
	if rerr := os.Remove(u.path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		err = errors.Join(err, rerr)
	}
	if err != nil {
		return fmt.Errorf("abort upload: %w", err)
	}
	return nil
}
