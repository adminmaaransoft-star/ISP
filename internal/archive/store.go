// Package archive implements document archival to external storage and the
// retention sweep that eventually removes it.
//
// FR: FR-DOC-001 | migration 034 | MDS §4.24.
//
// Three pieces, deliberately separable:
//
//   - A Store, which puts bytes somewhere durable and can delete them again.
//     Local filesystem is implemented here; the interface is the seam an S3 or
//     SFTP backend slots into without the archival logic changing.
//   - An Archiver, which streams a document into a Store, records where it
//     went with a checksum, and decides when it may be deleted.
//   - A PurgeScanner, which actually deletes it when that day comes.
//
// The checksum is the part that makes this archival rather than copying. A
// copy nobody can verify is a copy nobody can rely on in the dispute that
// caused someone to go looking for it years later.
package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Backend names match the storage_backend CHECK constraint in migration 034.
const (
	BackendLocal = "local"
	BackendS3    = "s3"
	BackendSFTP  = "sftp"
)

// PutResult describes what a Store wrote.
type PutResult struct {
	// URL locates the object well enough for the same Store to delete or
	// retrieve it later. Its shape is backend-specific and opaque to callers.
	URL string
	// SizeBytes and ChecksumSHA256 are measured over the bytes as written, not
	// as offered: a truncated upload that reported the source's length would be
	// a corrupt archive that verifies against the wrong number.
	SizeBytes      int64
	ChecksumSHA256 string
}

// Store is the storage driver seam.
type Store interface {
	// Put streams r to key, returning where it landed. Implementations must be
	// safe to call twice with the same key — a retry after a network failure is
	// normal — and must not leave a partial object visible under key if they
	// fail midway.
	Put(ctx context.Context, key string, r io.Reader) (*PutResult, error)
	// Get opens a previously-Put object for reading, addressed by the URL Put
	// returned. The caller must Close it. Checksum verification is not this
	// method's job — it returns exactly what is stored, corrupt or not — so
	// that Archiver.Retrieve can tell "the file is wrong" apart from "the file
	// could not be read" instead of this layer collapsing the two.
	Get(ctx context.Context, url string) (io.ReadCloser, error)
	// Delete removes a previously-Put object. Deleting something already gone
	// is not an error: the purge scanner retries, and a backend that reported
	// failure for an object it had already removed would retry forever.
	Delete(ctx context.Context, url string) error
	// Backend names this driver for the storage_backend column.
	Backend() string
}

// ── Local filesystem ────────────────────────────────────────────────────────

// LocalStore writes under a single root directory.
//
// Intended for single-node deployments and for development, and it is honest
// about what it is: a copy on the same machine is not disaster recovery. The
// value it does provide is the same as any backend here — documents move out
// of the database, get a checksum, and acquire a retention date.
type LocalStore struct {
	root string
	// dirPerm/filePerm keep archived documents unreadable to other local users.
	// These are invoice PDFs and KYC scans; 0644 would expose them to every
	// account on the host.
	dirPerm  os.FileMode
	filePerm os.FileMode
}

var _ Store = (*LocalStore)(nil)

// NewLocalStore roots a LocalStore at dir, creating it if absent.
func NewLocalStore(dir string) (*LocalStore, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("archive: resolve root %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("archive: create root %q: %w", abs, err)
	}
	return &LocalStore{root: abs, dirPerm: 0o750, filePerm: 0o640}, nil
}

// Backend implements Store.
func (s *LocalStore) Backend() string { return BackendLocal }

// Put writes r to root/key atomically.
//
// Written to a temporary file and renamed into place, so a crash or a full
// disk halfway through leaves no partial file under the real key. rename is
// atomic within a filesystem, which is what makes "the object exists" and "the
// object is complete" the same statement.
func (s *LocalStore) Put(ctx context.Context, key string, r io.Reader) (*PutResult, error) {
	rel, err := s.safePath(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(rel), s.dirPerm); err != nil {
		return nil, fmt.Errorf("archive: create directory for %q: %w", key, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(rel), ".partial-*")
	if err != nil {
		return nil, fmt.Errorf("archive: create temp file for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	// Removed on every failure path below. On success the rename has already
	// consumed it and this is a no-op.
	defer func() {
		_ = tmp.Close()        //nolint:errcheck
		_ = os.Remove(tmpName) //nolint:errcheck
	}()

	if err := tmp.Chmod(s.filePerm); err != nil {
		return nil, fmt.Errorf("archive: set permissions on %q: %w", key, err)
	}

	// Hash while writing rather than re-reading afterwards: r may be a network
	// stream that cannot be rewound, and a second pass over a large PDF is work
	// for nothing.
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), readerWithContext(ctx, r))
	if err != nil {
		return nil, fmt.Errorf("archive: write %q: %w", key, err)
	}
	if size == 0 {
		// The schema requires size_bytes > 0, and an empty archive is a
		// successful-looking record of nothing.
		return nil, fmt.Errorf("archive: refusing to archive an empty document for %q", key)
	}
	// Durability before visibility: without the sync, the rename can be
	// recorded while the contents are still only in the page cache, and a power
	// loss leaves a correctly-named empty file.
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("archive: sync %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("archive: close %q: %w", key, err)
	}
	if err := os.Rename(tmpName, rel); err != nil {
		return nil, fmt.Errorf("archive: commit %q: %w", key, err)
	}

	return &PutResult{
		URL:            "file://" + filepath.ToSlash(rel),
		SizeBytes:      size,
		ChecksumSHA256: hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// Get opens the object at url for reading.
//
// url is re-validated against the archive root the same way Delete's is —
// storage_url comes back out of the database, and a purge or a restore that
// trusted it unchecked would act on whatever a tampered row pointed at.
func (s *LocalStore) Get(_ context.Context, url string) (io.ReadCloser, error) {
	path, err := s.pathFromURL(url)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // path re-validated by pathFromURL against s.root
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("archive: %q: %w", url, os.ErrNotExist)
		}
		return nil, fmt.Errorf("archive: open %q: %w", url, err)
	}
	return f, nil
}

// Delete removes the object at url. A missing file is success.
func (s *LocalStore) Delete(_ context.Context, url string) error {
	path, err := s.pathFromURL(url)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("archive: delete %q: %w", url, err)
	}
	return nil
}

// safePath resolves key under the root and refuses anything that escapes it.
//
// Keys are built from document metadata, but "built from metadata" has been the
// preamble to a directory-traversal bug often enough to be worth checking
// rather than trusting: a doc kind or filename carrying ".." would otherwise
// let an archival write land anywhere the process can reach.
func (s *LocalStore) safePath(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("archive: empty storage key")
	}
	clean := filepath.Clean(filepath.Join(s.root, filepath.FromSlash(key)))
	if clean != s.root && !strings.HasPrefix(clean, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive: storage key %q escapes the archive root", key)
	}
	return clean, nil
}

// pathFromURL reverses Put's URL construction, re-checking containment so a
// tampered storage_url cannot direct a purge at an arbitrary file.
func (s *LocalStore) pathFromURL(url string) (string, error) {
	if !strings.HasPrefix(url, "file://") {
		return "", fmt.Errorf("archive: %q is not a local archive URL", url)
	}
	path := filepath.FromSlash(strings.TrimPrefix(url, "file://"))
	clean := filepath.Clean(path)
	if clean != s.root && !strings.HasPrefix(clean, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive: %q is outside the archive root", url)
	}
	return clean, nil
}

// readerWithContext aborts a long copy when ctx is cancelled.
//
// io.Copy has no notion of cancellation, so a shutdown during a large upload
// would otherwise block until the copy finished on its own.
func readerWithContext(ctx context.Context, r io.Reader) io.Reader {
	return &ctxReader{ctx: ctx, r: r}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
