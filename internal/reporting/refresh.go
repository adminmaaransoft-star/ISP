// Package reporting keeps the materialised reporting views current.
//
// Three of the four objects migration 032 creates are plain views and need
// nothing from this package — they are computed on read and are never stale.
// mv_ticket_resolution is materialised because it computes a percentile
// across every ticket ever filed, which is not a per-page-load query, and a
// materialised view with nothing refreshing it reports the numbers that were
// true on the day it was created, forever, without any outward sign.
//
// FR: FR-RPT-001 | MDS §4.8 (extended) | DBD §6.7
package reporting

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

var (
	refreshRuns = promauto.NewCounter(prometheus.CounterOpts{
		Name: "reporting_matview_refresh_total",
		Help: "Materialised reporting view refreshes attempted",
	})
	refreshFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "reporting_matview_refresh_failures_total",
		Help: "Materialised reporting view refreshes that failed",
	})
	// The gauge an alert should watch. A refresh that stops happening leaves
	// a dashboard showing confident, plausible, wrong numbers — the failure
	// mode with no visible symptom, so staleness has to be measurable.
	lastRefresh = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "reporting_matview_last_refresh_timestamp",
		Help: "Unix timestamp of the last successful reporting view refresh",
	})
)

// Refresher is the persistence surface the scanner needs.
type Refresher interface {
	RefreshTicketResolution(ctx context.Context) error
}

// RefreshScanner keeps mv_ticket_resolution current.
type RefreshScanner struct {
	db       Refresher
	interval time.Duration
}

// DefaultInterval is how often the ticket-resolution view is rebuilt.
//
// Fifteen minutes is chosen against what the numbers are used for: monthly
// medians and SLA attainment do not move meaningfully minute to minute, and a
// support lead looking at this morning's figures is well served by a quarter
// hour of lag. Refreshing far more often would spend real CPU recomputing a
// percentile over the whole ticket table to change a number in the second
// decimal place.
const DefaultInterval = 15 * time.Minute

// NewRefreshScanner constructs the scanner. A zero interval takes the default.
func NewRefreshScanner(db Refresher, interval time.Duration) *RefreshScanner {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &RefreshScanner{db: db, interval: interval}
}

// Run refreshes immediately, then on the interval, until ctx is cancelled.
//
// It runs once at startup, unlike the revenue reconciler: this view holds no
// date-keyed snapshot that an early run could overwrite, and a process that
// has just come up after a deploy is exactly when the view is most likely to
// be stale.
func (s *RefreshScanner) Run(ctx context.Context) {
	if err := s.Scan(ctx); err != nil {
		log.Error().Err(err).Msg("reporting: initial view refresh failed")
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Scan(ctx); err != nil {
				// Logged, not fatal. A scanner that exited on one bad refresh
				// would silently stop updating reporting from then on, which
				// is the same invisible staleness this package exists to
				// prevent.
				log.Error().Err(err).Msg("reporting: view refresh failed")
			}
		}
	}
}

// Scan performs one refresh. Exported so a test can drive a single pass
// without waiting on the ticker.
func (s *RefreshScanner) Scan(ctx context.Context) error {
	refreshRuns.Inc()
	if err := s.db.RefreshTicketResolution(ctx); err != nil {
		refreshFailures.Inc()
		return fmt.Errorf("reporting: refresh ticket resolution: %w", err)
	}
	lastRefresh.SetToCurrentTime()
	return nil
}
