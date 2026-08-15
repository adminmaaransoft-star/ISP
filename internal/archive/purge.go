package archive

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// Retention purge — FR-DOC-001 | MDS §4.24.
//
// The half of retention that actually does something. A retain_until date with
// nothing enforcing it is a policy document, not a control: under the DPDP
// Act's storage-limitation principle, keeping a KYC scan past its retention
// period is the violation, and a column recording the date it should have gone
// makes that worse rather than better by proving the system knew.

var (
	purgedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "document_purged_total",
		Help: "Archived documents purged after retention, by kind and outcome",
	}, []string{"kind", "outcome"})
	purgeBacklog = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "document_purge_backlog",
		Help: "Archives past their retention date and not yet purged",
	})
)

const (
	defaultPurgeInterval = 6 * time.Hour
	// defaultPurgeBatch bounds one sweep. A backlog — the first run after this
	// feature ships, say — is worked through over successive passes rather than
	// in one transaction holding thousands of rows while deleting from storage.
	defaultPurgeBatch = 200
)

// PurgeScanner deletes archives whose retention has expired.
type PurgeScanner struct {
	store    Store
	db       Recorder
	interval time.Duration
	batch    int
}

// NewPurgeScanner constructs a PurgeScanner. An interval of 0 uses the default.
func NewPurgeScanner(store Store, recorder Recorder, interval time.Duration) *PurgeScanner {
	if interval <= 0 {
		interval = defaultPurgeInterval
	}
	return &PurgeScanner{store: store, db: recorder, interval: interval, batch: defaultPurgeBatch}
}

// SetBatchSize overrides how many archives one sweep processes.
func (s *PurgeScanner) SetBatchSize(n int) {
	if n > 0 {
		s.batch = n
	}
}

// Run sweeps on an interval until ctx is cancelled.
//
// The first sweep happens immediately rather than after one interval: on a
// six-hour timer, waiting means a process that restarts every few hours never
// purges anything at all.
func (s *PurgeScanner) Run(ctx context.Context) {
	if err := s.PurgeOnce(ctx); err != nil {
		log.Error().Err(err).Msg("archive: initial retention sweep failed")
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.PurgeOnce(ctx); err != nil {
				log.Error().Err(err).Msg("archive: retention sweep failed")
			}
		}
	}
}

// PurgeOnce performs a single sweep, returning the number of archives purged.
//
// Storage first, then the row — the opposite order from archival, and for the
// same reason in reverse. Marking the row purged before the bytes are gone
// would leave a document on disk that the system believes it has deleted, and
// nothing would ever look at it again. This way a failure between the two
// leaves a row still due, and the next sweep retries the delete; deleting an
// object that is already gone is defined to succeed.
func (s *PurgeScanner) PurgeOnce(ctx context.Context) error {
	due, err := s.db.ListDueForPurge(ctx, s.batch)
	if err != nil {
		return err
	}
	purgeBacklog.Set(float64(len(due)))

	purged := 0
	for _, rec := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.store.Delete(ctx, rec.StorageURL); err != nil {
			purgedTotal.WithLabelValues(rec.DocKind, "storage_error").Inc()
			log.Error().Err(err).Int64("archive_id", rec.ID).Str("url", rec.StorageURL).
				Msg("archive: could not delete archived document; will retry next sweep")
			continue
		}
		marked, err := s.db.MarkPurged(ctx, rec.ID)
		if err != nil {
			// The bytes are already gone, so the row is now wrong until the mark
			// succeeds. Logged loudly because a repeat of this is the one state
			// where the record and reality genuinely disagree.
			purgedTotal.WithLabelValues(rec.DocKind, "record_error").Inc()
			log.Error().Err(err).Int64("archive_id", rec.ID).
				Msg("archive: document deleted from storage but purge not recorded")
			continue
		}
		if !marked {
			// Another replica's sweep got there first.
			purgedTotal.WithLabelValues(rec.DocKind, "already_purged").Inc()
			continue
		}
		purgedTotal.WithLabelValues(rec.DocKind, "purged").Inc()
		purged++
	}

	if purged > 0 {
		log.Info().Int("purged", purged).Int("due", len(due)).
			Msg("archive: retention sweep complete")
	}
	return nil
}
