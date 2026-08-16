//go:build integration

// Document archival persistence — FR-DOC-001 | migration 034 | MDS §4.24.
//
// The archive package's own tests use an in-memory recorder, so what is left
// to prove is the SQL: that the partial unique index actually collapses a
// re-archive into one live row, that the purge query selects only what is due,
// and that the conditional mark is safe when two sweeps race.
package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/archive"
)

func archiveRecord(kind string, entityID int, retainUntil *time.Time) archive.Record {
	return archive.Record{
		DocKind:        kind,
		EntityID:       entityID,
		StorageBackend: archive.BackendLocal,
		StorageURL:     "file:///srv/archive/" + kind + "/x.bin",
		ChecksumSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		SizeBytes:      1024,
		ArchivedAt:     time.Now(),
		RetainUntil:    retainUntil,
	}
}

// TestFR_DOC_001_ReArchivingKeepsOneLiveRow exercises the partial unique index.
// A retry after a network failure must not leave two rows each claiming to be
// the copy — a restore would then have to guess which is real.
func TestFR_DOC_001_ReArchivingKeepsOneLiveRow(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	store := database.Archive()

	first, err := store.RecordArchive(ctx, archiveRecord(archive.KindInvoice, 5, nil))
	if err != nil {
		t.Fatalf("first RecordArchive: %v", err)
	}

	updated := archiveRecord(archive.KindInvoice, 5, nil)
	updated.StorageURL = "file:///srv/archive/invoice/regenerated.pdf"
	updated.ChecksumSHA256 = "1111111111111111111111111111111111111111111111111111111111111111"
	updated.SizeBytes = 2048

	second, err := store.RecordArchive(ctx, updated)
	if err != nil {
		t.Fatalf("second RecordArchive: %v", err)
	}
	if first != second {
		t.Errorf("re-archiving one document must update its live row, got ids %d and %d", first, second)
	}

	rows := countRows(ctx, t, pool, `SELECT COUNT(*) FROM document_archives WHERE purged_at IS NULL`)
	if rows != 1 {
		t.Fatalf("want 1 live archive row, got %d", rows)
	}

	// The newest location and checksum win: the old checksum would no longer
	// verify against the object that is actually there.
	got, err := store.GetArchive(ctx, archive.KindInvoice, 5)
	if err != nil || got == nil {
		t.Fatalf("GetArchive: %v", err)
	}
	if got.StorageURL != updated.StorageURL || got.SizeBytes != 2048 {
		t.Errorf("the live row must describe the most recent archive, got %+v", got)
	}
}

// TestFR_DOC_001_PurgeQuerySelectsOnlyExpired is the retention boundary in SQL.
func TestFR_DOC_001_PurgeQuerySelectsOnlyExpired(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.Archive()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)

	expired, err := store.RecordArchive(ctx, archiveRecord(archive.KindReport, 1, &past))
	if err != nil {
		t.Fatalf("record expired: %v", err)
	}
	if _, err := store.RecordArchive(ctx, archiveRecord(archive.KindInvoice, 2, &future)); err != nil {
		t.Fatalf("record future: %v", err)
	}
	// No retention date at all — kept indefinitely, not purged immediately.
	if _, err := store.RecordArchive(ctx, archiveRecord(archive.KindKYC, 3, nil)); err != nil {
		t.Fatalf("record indefinite: %v", err)
	}

	due, err := store.ListDueForPurge(ctx, 100)
	if err != nil {
		t.Fatalf("ListDueForPurge: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("want exactly the expired archive, got %d: %+v", len(due), due)
	}
	if due[0].ID != expired {
		t.Errorf("want archive %d, got %d", expired, due[0].ID)
	}
	if due[0].StorageURL == "" || due[0].ChecksumSHA256 == "" {
		t.Error("the purge needs the storage URL to delete the object")
	}
}

// TestFR_DOC_001_MarkPurgedIsAConditionalClaim — two replicas run the sweep,
// and both must not count the same purge. The loser needs to know it lost
// rather than overwriting when the document actually went.
func TestFR_DOC_001_MarkPurgedIsAConditionalClaim(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	store := database.Archive()

	past := time.Now().Add(-time.Hour)
	id, err := store.RecordArchive(ctx, archiveRecord(archive.KindReport, 1, &past))
	if err != nil {
		t.Fatalf("RecordArchive: %v", err)
	}

	const racers = 8
	results := make(chan bool, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			ok, markErr := store.MarkPurged(ctx, id)
			if markErr != nil {
				t.Errorf("MarkPurged: %v", markErr)
			}
			results <- ok
		}()
	}
	close(start)

	won := 0
	for i := 0; i < racers; i++ {
		if <-results {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("exactly one sweep may claim a purge, %d of %d did", won, racers)
	}

	// And it drops out of the due list rather than being swept forever.
	due, err := store.ListDueForPurge(ctx, 100)
	if err != nil {
		t.Fatalf("ListDueForPurge: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("a purged archive must not remain due, got %d", len(due))
	}
	purged := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM document_archives WHERE purged_at IS NOT NULL`)
	if purged != 1 {
		t.Errorf("want 1 purged row, got %d", purged)
	}
}

// TestFR_DOC_001_PurgedRowFreesTheUniqueIndex — the unique index is partial on
// purged_at IS NULL, so a document archived again after being purged must be
// able to take a fresh row rather than colliding with the tombstone.
func TestFR_DOC_001_PurgedRowFreesTheUniqueIndex(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	store := database.Archive()

	past := time.Now().Add(-time.Hour)
	first, err := store.RecordArchive(ctx, archiveRecord(archive.KindInvoice, 9, &past))
	if err != nil {
		t.Fatalf("RecordArchive: %v", err)
	}
	if ok, err := store.MarkPurged(ctx, first); err != nil || !ok {
		t.Fatalf("MarkPurged: ok=%v err=%v", ok, err)
	}

	second, err := store.RecordArchive(ctx, archiveRecord(archive.KindInvoice, 9, nil))
	if err != nil {
		t.Fatalf("re-archive after purge: %v — the tombstone must not block a new copy", err)
	}
	if second == first {
		t.Error("a re-archive after purge must be a new row, not a resurrection of the purged one")
	}

	total := countRows(ctx, t, pool, `SELECT COUNT(*) FROM document_archives WHERE doc_kind='invoice' AND entity_id=9`)
	if total != 2 {
		t.Errorf("want the purged row kept as history plus the new one, got %d", total)
	}
	// GetArchive returns only the live one.
	got, err := store.GetArchive(ctx, archive.KindInvoice, 9)
	if err != nil || got == nil {
		t.Fatalf("GetArchive: %v", err)
	}
	if got.ID != second {
		t.Errorf("GetArchive must return the live archive %d, got %d", second, got.ID)
	}
}

// TestNFR_DUR_002_RetentionFloorIsEnforcedAtTheDatabase — NFR-DUR-002.
//
// MarkPurged always sets purged_at to NOW(), so the application code cannot
// currently produce an early purge on its own. That is exactly why this test
// bypasses it and issues the UPDATE directly: the guarantee that matters is
// that the database itself refuses the write, so a future bug in application
// code — a batch job, a manual fix, a different code path added later — hits
// the same wall rather than relying on every caller getting it right.
func TestNFR_DUR_002_RetentionFloorIsEnforcedAtTheDatabase(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	store := database.Archive()

	future := time.Now().Add(365 * 24 * time.Hour)
	id, err := store.RecordArchive(ctx, archiveRecord(archive.KindInvoice, 1, &future))
	if err != nil {
		t.Fatalf("RecordArchive: %v", err)
	}

	_, err = pool.Exec(ctx,
		`UPDATE document_archives SET purged_at = NOW() WHERE id = $1`, id)
	if err == nil {
		t.Fatal("purging before retain_until must be rejected by chk_archive_not_purged_before_retention " +
			"— a row with no policy behind it, purged early by a bug elsewhere, is exactly the case " +
			"this constraint exists to catch regardless of what application code did or didn't check")
	}
	if !strings.Contains(err.Error(), "chk_archive_not_purged_before_retention") {
		t.Errorf("want a rejection naming the retention constraint, got: %v", err)
	}

	// The legitimate case must still work: purging at or after retain_until.
	past := time.Now().Add(-time.Hour)
	dueID, err := store.RecordArchive(ctx, archiveRecord(archive.KindReport, 2, &past))
	if err != nil {
		t.Fatalf("RecordArchive (due): %v", err)
	}
	if ok, err := store.MarkPurged(ctx, dueID); err != nil || !ok {
		t.Fatalf("MarkPurged on a due archive must succeed: ok=%v err=%v", ok, err)
	}
}

// TestFR_DOC_001_UnarchivedDocumentReportsAbsence — nil rather than an error,
// so a caller can ask "is this archived yet" without treating no as a failure.
func TestFR_DOC_001_UnarchivedDocumentReportsAbsence(t *testing.T) {
	database, _ := newTestDB(t)

	got, err := database.Archive().GetArchive(context.Background(), archive.KindInvoice, 404)
	if err != nil {
		t.Fatalf("GetArchive: %v", err)
	}
	if got != nil {
		t.Errorf("want nil for a document that was never archived, got %+v", got)
	}
}
