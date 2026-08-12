package tickets

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// SLA breach detection — FR-SUP-002 | MDS §4.13.
//
// Tickets carry two independent deadlines (response and resolution,
// migration 023). This scanner notices when either is approaching or has
// passed and raises an alert exactly once per ticket per event type. It
// follows the same shape as fup.Scanner and billing.DunningScanner: a
// ticker loop owned by radiusd, not a new pattern.

var (
	slaEventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sla_events_total",
		Help: "SLA warnings and breaches recorded, by event type",
	}, []string{"event_type"})

	slaScanDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "sla_scan_duration_seconds",
		Help:    "Duration of one SLA breach scan",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5},
	})
)

const (
	// slaScanInterval is minutes, not seconds: SLA windows are measured in
	// hours (migration 023 seeds 15 minutes as the tightest response
	// target), so fup.Scanner's 10-second cadence would be pure load for no
	// extra precision. billing.DunningScanner's hourly tick is the closer
	// analog; this is tighter so a 15-minute response target cannot be
	// missed by most of its own window.
	slaScanInterval = 5 * time.Minute

	// slaWarningFraction is how much of a window must elapse before a
	// warning fires — 80%, the same threshold FR-FUP-004 already uses for
	// quota warnings. One convention for "nearly out of budget", not two.
	slaWarningFraction = 0.8
)

// SLAEvent is one newly-recorded threshold crossing.
type SLAEvent struct {
	TicketID     int
	EventType    string
	SubscriberID int
	Priority     string
	// RoutedRole is the staff role the ticket was routed to at creation, or
	// nil when no routing rule matched (MDS §4.13 — an unrouted ticket is a
	// normal outcome, not an error).
	RoutedRole *string
}

// IsBreach distinguishes a passed deadline from an approaching one.
func (e SLAEvent) IsBreach() bool {
	return e.EventType == "response_breach" || e.EventType == "resolution_breach"
}

// SLAQuerier is the persistence surface the scanner needs. Satisfied by
// *db.SLAStore.
type SLAQuerier interface {
	// ClaimSLAEvents records every newly-crossed threshold and returns only
	// what this call recorded — the uniqueness constraint behind it is what
	// makes an alert fire once rather than every five minutes forever.
	ClaimSLAEvents(ctx context.Context, warningFraction float64) ([]SLAEvent, error)
}

// SLAAlerter receives breach notifications. Deliberately the same shape as
// fup.Alerter, which radiusd already wires to logAlerter{} — staff_users
// has no email or phone column, so a per-staff notification channel does not
// exist to send to yet (MDS §4.13). Inventing one here would be scope creep
// into FR-NOTIF-012; routing through the existing alert path means breaches
// surface today through the same channel dead-letter alerts already use.
type SLAAlerter interface {
	Trigger(event string, detail any)
}

// SLAScanner periodically records SLA warnings and breaches.
type SLAScanner struct {
	db      SLAQuerier
	alerter SLAAlerter
}

// NewSLAScanner constructs an SLAScanner.
func NewSLAScanner(db SLAQuerier, alerter SLAAlerter) *SLAScanner {
	return &SLAScanner{db: db, alerter: alerter}
}

// Run scans every slaScanInterval until ctx is cancelled.
//
// Scans once immediately, matching DunningScanner: after a deployment that
// has never run this, waiting five minutes to notice an already-breached
// ticket serves nobody.
func (s *SLAScanner) Run(ctx context.Context) {
	if err := s.Scan(ctx); err != nil {
		log.Error().Err(err).Msg("sla: initial scan failed")
	}

	ticker := time.NewTicker(slaScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Scan(ctx); err != nil {
				log.Error().Err(err).Msg("sla: scan failed")
			}
		}
	}
}

// Scan records every threshold crossed since the last scan and alerts on
// each. Exported so a test can drive one pass without the ticker.
func (s *SLAScanner) Scan(ctx context.Context) error {
	timer := prometheus.NewTimer(slaScanDuration)
	defer timer.ObserveDuration()

	events, err := s.db.ClaimSLAEvents(ctx, slaWarningFraction)
	if err != nil {
		return err
	}

	for _, e := range events {
		slaEventsTotal.WithLabelValues(e.EventType).Inc()

		routed := "unrouted"
		if e.RoutedRole != nil {
			routed = *e.RoutedRole
		}

		// Warnings are logged; breaches also fire the alerter. A ticket at
		// 80% of its window is information for whoever is watching, not an
		// incident — alerting on both would train staff to ignore the
		// channel, which is how a breach alert stops working.
		if !e.IsBreach() {
			log.Warn().
				Int("ticket_id", e.TicketID).
				Str("event", e.EventType).
				Str("priority", e.Priority).
				Str("routed_role", routed).
				Msg("sla: ticket approaching its SLA deadline")
			continue
		}

		log.Error().
			Int("ticket_id", e.TicketID).
			Str("event", e.EventType).
			Str("priority", e.Priority).
			Str("routed_role", routed).
			Msg("sla: ticket has breached its SLA deadline")

		if s.alerter != nil {
			s.alerter.Trigger(e.EventType, map[string]any{
				"ticket_id":     e.TicketID,
				"subscriber_id": e.SubscriberID,
				"priority":      e.Priority,
				"routed_role":   routed,
			})
		}
	}
	return nil
}
