package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/archive"
)

// Document archival persistence — FR-DOC-001 | migration 034 | MDS §4.24.

// ArchiveStore reads and writes document_archives.
type ArchiveStore struct{ pool dbPool }

var _ archive.Recorder = (*ArchiveStore)(nil)

// RecordArchive stores where a document was archived, returning its id.
//
// Upserts on the partial unique index (doc_kind, entity_id, storage_backend)
// WHERE purged_at IS NULL. Re-archiving the same document — a retry after a
// network failure, or a regenerated invoice PDF — must not create a second live
// row claiming a second copy that does not exist. The conflict target repeats
// the index predicate because PostgreSQL requires it to match a partial index.
//
// On conflict the new location, checksum and size win: the bytes that were just
// written are the ones that exist now, and keeping the previous checksum would
// make the row fail verification against its own object.
func (s *ArchiveStore) RecordArchive(ctx context.Context, r archive.Record) (int64, error) {
	const q = `
		INSERT INTO document_archives (
			doc_kind, entity_id, storage_backend, storage_url,
			checksum_sha256, size_bytes, archived_at, retain_until)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (doc_kind, entity_id, storage_backend) WHERE purged_at IS NULL
		DO UPDATE SET
			storage_url     = EXCLUDED.storage_url,
			checksum_sha256 = EXCLUDED.checksum_sha256,
			size_bytes      = EXCLUDED.size_bytes,
			archived_at     = EXCLUDED.archived_at,
			retain_until    = EXCLUDED.retain_until
		RETURNING id`

	var id int64
	err := s.pool.QueryRow(ctx, q, r.DocKind, r.EntityID, r.StorageBackend, r.StorageURL,
		r.ChecksumSHA256, r.SizeBytes, r.ArchivedAt, r.RetainUntil).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: record archive of %s %d: %w", r.DocKind, r.EntityID, err)
	}
	return id, nil
}

// ListDueForPurge returns archives past their retention date, oldest first.
//
// A NULL retain_until is excluded rather than treated as "purge now": it means
// no policy named an interval for that kind, and the safe reading of an absent
// retention rule is to keep the document, not to delete it.
func (s *ArchiveStore) ListDueForPurge(ctx context.Context, limit int) ([]archive.Record, error) {
	const q = `
		SELECT id, doc_kind, entity_id, storage_backend, storage_url,
		       checksum_sha256, size_bytes, archived_at, retain_until, purged_at
		  FROM document_archives
		 WHERE purged_at IS NULL
		   AND retain_until IS NOT NULL
		   AND retain_until <= NOW()
		 ORDER BY retain_until
		 LIMIT $1`

	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list archives due for purge: %w", err)
	}
	defer rows.Close()

	out := make([]archive.Record, 0, limit)
	for rows.Next() {
		var r archive.Record
		if err := rows.Scan(&r.ID, &r.DocKind, &r.EntityID, &r.StorageBackend, &r.StorageURL,
			&r.ChecksumSHA256, &r.SizeBytes, &r.ArchivedAt, &r.RetainUntil, &r.PurgedAt); err != nil {
			return nil, fmt.Errorf("db: scan archive row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate archives: %w", err)
	}
	return out, nil
}

// MarkPurged records that an archive's bytes are gone, reporting whether this
// call was the one that did it.
//
// The purged_at IS NULL predicate makes the mark a conditional claim, the same
// pattern vouchers and approvals use: two replicas sweeping at once must not
// both count a purge, and the loser needs to know it lost rather than
// overwriting the timestamp of when the document actually went.
//
// NOW() rather than a caller-supplied time because the schema's
// chk_archive_not_purged_before_retention constraint compares purged_at to
// retain_until, and a clock-skewed application server could otherwise write a
// timestamp the database rejects.
func (s *ArchiveStore) MarkPurged(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE document_archives SET purged_at = NOW() WHERE id = $1 AND purged_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("db: mark archive %d purged: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetArchive returns the live archive row for a document, or nil when it has
// never been archived (or has been purged).
func (s *ArchiveStore) GetArchive(ctx context.Context, docKind string, entityID int) (*archive.Record, error) {
	const q = `
		SELECT id, doc_kind, entity_id, storage_backend, storage_url,
		       checksum_sha256, size_bytes, archived_at, retain_until, purged_at
		  FROM document_archives
		 WHERE doc_kind = $1 AND entity_id = $2 AND purged_at IS NULL
		 ORDER BY archived_at DESC
		 LIMIT 1`

	var r archive.Record
	err := s.pool.QueryRow(ctx, q, docKind, entityID).Scan(
		&r.ID, &r.DocKind, &r.EntityID, &r.StorageBackend, &r.StorageURL,
		&r.ChecksumSHA256, &r.SizeBytes, &r.ArchivedAt, &r.RetainUntil, &r.PurgedAt)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get archive of %s %d: %w", docKind, entityID, err)
	}
	return &r, nil
}
