// Document archival tests — FR-DOC-001 | MDS §4.24.
//
// Two properties carry most of the weight here. An archive that cannot be
// verified is not an archive, so the checksum has to be over the bytes as
// written. And a retention date with nothing enforcing it is worse than no
// date at all, so the purge has to actually delete — in the right order, and
// only when due.
package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Test doubles ────────────────────────────────────────────────────────────

// memRecorder is an in-memory Recorder.
type memRecorder struct {
	mu sync.Mutex

	records map[int64]*Record
	nextID  int64
	purged  []int64
	recErr  error
	listErr error
	markErr error
}

func newMemRecorder() *memRecorder {
	return &memRecorder{records: map[int64]*Record{}}
}

func (m *memRecorder) RecordArchive(_ context.Context, r Record) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recErr != nil {
		return 0, m.recErr
	}
	// Mirrors the partial unique index: one live row per
	// (kind, entity, backend).
	for id, existing := range m.records {
		if existing.PurgedAt == nil && existing.DocKind == r.DocKind &&
			existing.EntityID == r.EntityID && existing.StorageBackend == r.StorageBackend {
			r.ID = id
			m.records[id] = &r
			return id, nil
		}
	}
	m.nextID++
	r.ID = m.nextID
	m.records[m.nextID] = &r
	return m.nextID, nil
}

func (m *memRecorder) ListDueForPurge(_ context.Context, limit int) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []Record
	for _, r := range m.records {
		if r.PurgedAt == nil && r.RetainUntil != nil && !r.RetainUntil.After(time.Now()) {
			out = append(out, *r)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memRecorder) MarkPurged(_ context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markErr != nil {
		return false, m.markErr
	}
	r, ok := m.records[id]
	if !ok || r.PurgedAt != nil {
		return false, nil
	}
	now := time.Now()
	r.PurgedAt = &now
	m.purged = append(m.purged, id)
	return true, nil
}

func (m *memRecorder) liveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.records {
		if r.PurgedAt == nil {
			n++
		}
	}
	return n
}

func (m *memRecorder) purgedIDs() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.purged...)
}

// failingStore fails Delete, to check the purge does not mark rows it did not
// actually delete.
type failingStore struct {
	Store
	deleteErr error
	deleted   []string
	mu        sync.Mutex
}

func (f *failingStore) Delete(ctx context.Context, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, url)
	return f.Store.Delete(ctx, url)
}

func localStore(t *testing.T) *LocalStore {
	t.Helper()
	s, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	return s
}

// ── Storage driver ──────────────────────────────────────────────────────────

func TestFR_DOC_001_PutRecordsWhatItActuallyWrote(t *testing.T) {
	s := localStore(t)
	body := []byte("INVOICE PDF BYTES")

	res, err := s.Put(context.Background(), "invoice/2026/08/1-inv.pdf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if res.SizeBytes != int64(len(body)) {
		t.Errorf("size: want %d, got %d", len(body), res.SizeBytes)
	}
	want := sha256.Sum256(body)
	if res.ChecksumSHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("checksum must be over the archived bytes; got %s", res.ChecksumSHA256)
	}

	// And the bytes are really there, matching the checksum — the property that
	// makes a restore verifiable rather than hopeful.
	path := strings.TrimPrefix(res.URL, "file://")
	onDisk, err := os.ReadFile(filepath.FromSlash(path)) //nolint:gosec // path produced by this test
	if err != nil {
		t.Fatalf("read archived file: %v", err)
	}
	if !bytes.Equal(onDisk, body) {
		t.Error("archived bytes differ from the source")
	}
	got := sha256.Sum256(onDisk)
	if hex.EncodeToString(got[:]) != res.ChecksumSHA256 {
		t.Error("the recorded checksum does not verify against the stored object")
	}
}

// TestFR_DOC_001_StorageKeysCannotEscapeTheRoot is the traversal guard. Keys
// are assembled from document metadata, and metadata has been the source of
// enough path-traversal bugs to be worth refusing rather than trusting.
func TestFR_DOC_001_StorageKeysCannotEscapeTheRoot(t *testing.T) {
	s := localStore(t)

	for _, key := range []string{
		"../escaped.pdf",
		"invoice/../../escaped.pdf",
		"invoice/../../../../../../tmp/escaped.pdf",
		"",
	} {
		if _, err := s.Put(context.Background(), key, strings.NewReader("x")); err == nil {
			t.Errorf("key %q must be refused — an archival write outside the root can overwrite "+
				"anything the process can reach", key)
		}
	}

	// A traversal that stays inside the root is fine: this refuses escapes, not
	// every use of "..".
	if _, err := s.Put(context.Background(), "invoice/sub/../1-inv.pdf", strings.NewReader("x")); err != nil {
		t.Errorf("a key that normalises to somewhere inside the root must be accepted: %v", err)
	}
}

// TestFR_DOC_001_DeleteRefusesURLsOutsideTheRoot — storage_url comes back from
// the database, and a purge that trusted it blindly would delete whatever a
// tampered row pointed at.
func TestFR_DOC_001_DeleteRefusesURLsOutsideTheRoot(t *testing.T) {
	s := localStore(t)

	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatalf("write victim file: %v", err)
	}

	err := s.Delete(context.Background(), "file://"+filepath.ToSlash(outside))
	if err == nil {
		t.Error("deleting outside the archive root must be refused")
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Error("the file outside the root must still exist")
	}

	if err := s.Delete(context.Background(), "https://example.com/x"); err == nil {
		t.Error("a non-local URL must be refused by the local backend")
	}
}

func TestFR_DOC_001_EmptyDocumentIsRefused(t *testing.T) {
	s := localStore(t)
	if _, err := s.Put(context.Background(), "invoice/empty.pdf", strings.NewReader("")); err == nil {
		t.Error("an empty archive is a successful-looking record of nothing, and the schema's " +
			"size_bytes > 0 constraint would reject the row anyway")
	}
}

// TestFR_DOC_001_FailedWriteLeavesNoPartialObject — a crash mid-copy must not
// leave a truncated file under the real key, where it would look archived.
func TestFR_DOC_001_FailedWriteLeavesNoPartialObject(t *testing.T) {
	s := localStore(t)
	key := "invoice/2026/08/9-truncated.pdf"

	_, err := s.Put(context.Background(), key, io.MultiReader(
		strings.NewReader("first half"),
		&erroringReader{err: errors.New("connection reset")},
	))
	if err == nil {
		t.Fatal("a read failure mid-copy must fail the Put")
	}

	if _, statErr := os.Stat(filepath.Join(s.root, filepath.FromSlash(key))); !os.IsNotExist(statErr) {
		t.Error("a failed Put must leave nothing under the target key")
	}
	// Nor a stray temp file.
	entries, _ := os.ReadDir(filepath.Dir(filepath.Join(s.root, filepath.FromSlash(key)))) //nolint:errcheck
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".partial-") {
			t.Errorf("a failed Put left a temp file behind: %s", e.Name())
		}
	}
}

type erroringReader struct{ err error }

func (e *erroringReader) Read([]byte) (int, error) { return 0, e.err }

func TestFR_DOC_001_CancelledContextStopsTheCopy(t *testing.T) {
	s := localStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Put(ctx, "invoice/cancelled.pdf", strings.NewReader("data")); err == nil {
		t.Error("a cancelled context must abort the copy rather than finishing it")
	}
}

// ── Archiver ────────────────────────────────────────────────────────────────

func TestFR_DOC_001_ArchiveRecordsLocationChecksumAndRetention(t *testing.T) {
	store := localStore(t)
	rec := newMemRecorder()
	archiver := NewArchiver(store, rec)

	got, err := archiver.Archive(context.Background(), Document{
		Kind: KindInvoice, EntityID: 42, Filename: "INV-2026-0042.pdf",
		Body: strings.NewReader("pdf bytes"),
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if got.StorageBackend != BackendLocal {
		t.Errorf("backend: want %q, got %q", BackendLocal, got.StorageBackend)
	}
	if got.ChecksumSHA256 == "" || got.SizeBytes != 9 {
		t.Errorf("checksum/size not recorded: %+v", got)
	}
	if got.RetainUntil == nil {
		t.Fatal("an invoice must get a retention date — a document with no purge date is kept forever")
	}
	// 8 years for an invoice, per DefaultRetention.
	wantAbout := time.Now().AddDate(8, 0, 0)
	if diff := got.RetainUntil.Sub(wantAbout); diff > 48*time.Hour || diff < -48*time.Hour {
		t.Errorf("invoice retention: want about %v, got %v", wantAbout, *got.RetainUntil)
	}
	// The key is readable and identifies the document without the database.
	if !strings.Contains(got.StorageURL, "invoice/") || !strings.Contains(got.StorageURL, "42-INV-2026-0042.pdf") {
		t.Errorf("storage url should identify the document: %s", got.StorageURL)
	}
}

func TestFR_DOC_001_RetentionDiffersByKind(t *testing.T) {
	invoice := DefaultRetention.RetainUntil(KindInvoice, time.Now())
	kyc := DefaultRetention.RetainUntil(KindKYC, time.Now())
	report := DefaultRetention.RetainUntil(KindReport, time.Now())

	if invoice == nil || kyc == nil || report == nil {
		t.Fatal("every shipped kind must have a retention interval")
	}
	if !invoice.After(*kyc) {
		t.Error("GST record-keeping outlives KYC retention; a single interval would either " +
			"delete invoices early or hoard KYC scans")
	}
	if !kyc.After(*report) {
		t.Error("a regenerable report should not be kept as long as a KYC document")
	}

	// An unknown kind gets no date, and the purge scanner skips those: keeping
	// a document too long is a conversation, deleting it early can be final.
	if DefaultRetention.RetainUntil("something_else", time.Now()) != nil {
		t.Error("an unrecognised kind must not be given a purge date")
	}
}

func TestFR_DOC_001_ArchiveRejectsBadInput(t *testing.T) {
	archiver := NewArchiver(localStore(t), newMemRecorder())

	tests := []struct {
		name string
		doc  Document
	}{
		{"unknown kind", Document{Kind: "passport", EntityID: 1, Body: strings.NewReader("x")}},
		{"no entity", Document{Kind: KindInvoice, EntityID: 0, Body: strings.NewReader("x")}},
		{"no body", Document{Kind: KindInvoice, EntityID: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := archiver.Archive(context.Background(), tc.doc); err == nil {
				t.Error("expected a refusal")
			}
		})
	}
}

// TestFR_DOC_001_ReArchivingReplacesTheLiveRecord — a retry after a failure, or
// a regenerated invoice, must not leave two rows each claiming to be the copy.
func TestFR_DOC_001_ReArchivingReplacesTheLiveRecord(t *testing.T) {
	rec := newMemRecorder()
	archiver := NewArchiver(localStore(t), rec)

	first, err := archiver.Archive(context.Background(), Document{
		Kind: KindInvoice, EntityID: 7, Filename: "a.pdf", Body: strings.NewReader("v1")})
	if err != nil {
		t.Fatalf("first archive: %v", err)
	}
	second, err := archiver.Archive(context.Background(), Document{
		Kind: KindInvoice, EntityID: 7, Filename: "a.pdf", Body: strings.NewReader("v2 longer")})
	if err != nil {
		t.Fatalf("second archive: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("re-archiving one document must update its live row, got ids %d and %d", first.ID, second.ID)
	}
	if rec.liveCount() != 1 {
		t.Errorf("want 1 live archive row, got %d", rec.liveCount())
	}
	if first.ChecksumSHA256 == second.ChecksumSHA256 {
		t.Error("different content must produce a different checksum")
	}
}

// TestFR_DOC_001_FilenamesAreSanitised keeps a caller-supplied name from
// steering the key, independently of the Store's own containment check.
func TestFR_DOC_001_FilenamesAreSanitised(t *testing.T) {
	for _, name := range []string{
		"../../etc/passwd",
		`..\..\windows\system32`,
		"....//....//evil",
		"/absolute/path.pdf",
	} {
		got := sanitiseFilename(name)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitised %q still contains a separator: %q", name, got)
		}
		if strings.HasPrefix(got, ".") {
			t.Errorf("sanitised %q still starts with a dot: %q", name, got)
		}
	}
	if got := sanitiseFilename("INV-2026-0042.pdf"); got != "INV-2026-0042.pdf" {
		t.Errorf("an ordinary filename must survive intact, got %q", got)
	}
	if got := sanitiseFilename(strings.Repeat("a", 500)); len(got) != 100 {
		t.Errorf("an overlong filename must be bounded, got %d chars", len(got))
	}
}

// ── Purge ───────────────────────────────────────────────────────────────────

// TestFR_DOC_001_PurgeDeletesOnlyWhatIsDue is the core retention behaviour: the
// sweep must remove expired documents and leave everything else alone.
func TestFR_DOC_001_PurgeDeletesOnlyWhatIsDue(t *testing.T) {
	store := localStore(t)
	rec := newMemRecorder()
	archiver := NewArchiver(store, rec)
	ctx := context.Background()

	// Due: archived two years ago under a one-year retention. Backdating the
	// clock rather than using a negative retention, so this exercises the same
	// arithmetic production uses instead of a value the policy would reject.
	archiver.now = func() time.Time { return time.Now().AddDate(-2, 0, 0) }
	expired, err := archiver.Archive(ctx, Document{
		Kind: KindReport, EntityID: 1, Filename: "old.csv", Body: strings.NewReader("old")})
	if err != nil {
		t.Fatalf("archive expired: %v", err)
	}
	archiver.now = time.Now

	// Not due: years away.
	kept, err := archiver.Archive(ctx, Document{
		Kind: KindInvoice, EntityID: 2, Filename: "keep.pdf", Body: strings.NewReader("keep")})
	if err != nil {
		t.Fatalf("archive kept: %v", err)
	}

	// No retention at all: kept indefinitely.
	archiver.SetRetention(RetentionPolicy{})
	forever, err := archiver.Archive(ctx, Document{
		Kind: KindKYC, EntityID: 3, Filename: "kyc.jpg", Body: strings.NewReader("kyc")})
	if err != nil {
		t.Fatalf("archive forever: %v", err)
	}

	scanner := NewPurgeScanner(store, rec, time.Hour)
	if err := scanner.PurgeOnce(ctx); err != nil {
		t.Fatalf("PurgeOnce: %v", err)
	}

	assertGone(t, expired.StorageURL, "an archive past its retention date must be deleted")
	assertPresent(t, kept.StorageURL, "an archive inside its retention must be kept")
	assertPresent(t, forever.StorageURL, "an archive with no retention date must be kept")

	purged := rec.purgedIDs()
	if len(purged) != 1 || purged[0] != expired.ID {
		t.Errorf("exactly the expired archive must be marked purged, got %v", purged)
	}
}

// TestFR_DOC_001_PurgeDoesNotMarkWhatItFailedToDelete is the ordering property.
// Marking first would leave a document on disk that the system believes is
// gone — and nothing would ever look at it again.
func TestFR_DOC_001_PurgeDoesNotMarkWhatItFailedToDelete(t *testing.T) {
	base := localStore(t)
	rec := newMemRecorder()
	archiver := NewArchiver(base, rec)
	archiver.now = func() time.Time { return time.Now().AddDate(-2, 0, 0) }

	stored, err := archiver.Archive(context.Background(), Document{
		Kind: KindReport, EntityID: 1, Filename: "r.csv", Body: strings.NewReader("data")})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	broken := &failingStore{Store: base, deleteErr: errors.New("storage unavailable")}
	scanner := NewPurgeScanner(broken, rec, time.Hour)
	if err := scanner.PurgeOnce(context.Background()); err != nil {
		t.Fatalf("PurgeOnce must not fail the sweep over one bad delete: %v", err)
	}

	if len(rec.purgedIDs()) != 0 {
		t.Error("a document whose deletion failed must not be recorded as purged — the row would " +
			"claim it is gone while the bytes remain, and nothing would revisit it")
	}
	assertPresent(t, stored.StorageURL, "the document must still exist after a failed delete")

	// The next sweep retries and succeeds.
	working := NewPurgeScanner(base, rec, time.Hour)
	if err := working.PurgeOnce(context.Background()); err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	if len(rec.purgedIDs()) != 1 {
		t.Error("the next sweep must retry the failed purge")
	}
	assertGone(t, stored.StorageURL, "the retried purge must delete the document")
}

// TestFR_DOC_001_ConcurrentSweepsPurgeOnce — two replicas both run this
// scanner, and the conditional mark is what stops both counting the same purge.
func TestFR_DOC_001_ConcurrentSweepsPurgeOnce(t *testing.T) {
	store := localStore(t)
	rec := newMemRecorder()
	archiver := NewArchiver(store, rec)
	archiver.now = func() time.Time { return time.Now().AddDate(-2, 0, 0) }

	if _, err := archiver.Archive(context.Background(), Document{
		Kind: KindReport, EntityID: 1, Filename: "r.csv", Body: strings.NewReader("data")}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = NewPurgeScanner(store, rec, time.Hour).PurgeOnce(context.Background()) //nolint:errcheck
		}()
	}
	wg.Wait()

	if got := len(rec.purgedIDs()); got != 1 {
		t.Errorf("concurrent sweeps must purge one archive exactly once, got %d", got)
	}
}

func TestFR_DOC_001_PurgeSurvivesAListingFailure(t *testing.T) {
	rec := newMemRecorder()
	rec.listErr = errors.New("database unavailable")

	err := NewPurgeScanner(localStore(t), rec, time.Hour).PurgeOnce(context.Background())
	if err == nil {
		t.Error("a listing failure must be reported so the caller can log it")
	}
}

func assertGone(t *testing.T, url, msg string) {
	t.Helper()
	if _, err := os.Stat(pathOf(url)); !os.IsNotExist(err) {
		t.Error(msg)
	}
}

func assertPresent(t *testing.T, url, msg string) {
	t.Helper()
	if _, err := os.Stat(pathOf(url)); err != nil {
		t.Errorf("%s (%v)", msg, err)
	}
}

func pathOf(url string) string {
	return filepath.FromSlash(strings.TrimPrefix(url, "file://"))
}

// Compile-time proof the in-memory double matches the interface the real store
// implements, so these tests cannot drift from the production shape.
var _ Recorder = (*memRecorder)(nil)
