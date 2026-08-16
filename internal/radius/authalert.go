package radius

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
)

// Per-NAS authentication failure alerting — FR-OBS-005 | SAD §3.2.
//
// "Emit a proactive alert when the RADIUS auth failure rate on any NAS exceeds
// 20% over 5 minutes." Two things had to exist before that rule could be
// written at all: a per-NAS outcome counter (radius_auth_outcome_total, added
// alongside this) and something to evaluate it. The unlabelled totals this
// codebase had could express "failures are up" but not "on any NAS", which is
// the part that tells an operator where to look.
//
// Evaluated in process rather than by a Prometheus rule because this
// deployment has no Prometheus or Alertmanager — a rules file would have been
// a requirement satisfied on paper by a file nothing reads. The equivalent
// PromQL is in deploy/prometheus/radius_alerts.yml for deployments that do run
// one; both express the same threshold, and the file is the better answer once
// the infrastructure exists.

const (
	// authAlertWindow is the "over 5 min" the requirement names.
	authAlertWindow = 5 * time.Minute
	// authAlertInterval is how often the window is evaluated. Six samples
	// across the window is enough resolution to catch a NAS going bad without
	// sampling the registry every second.
	authAlertInterval = 50 * time.Second
	// AuthFailureThreshold is the failure rate that trips the alert.
	AuthFailureThreshold = 0.20
	// authAlertMinAttempts is the volume below which the rate is not
	// meaningful. Without it a single failed authentication on an idle NAS is
	// a 100% failure rate, and the alert fires constantly on the quietest
	// sites — which trains everyone to ignore it, leaving a real outage on a
	// busy NAS indistinguishable from the noise.
	authAlertMinAttempts = 20
	// AuthFailureAlertEvent is the event name the Alerter receives.
	AuthFailureAlertEvent = "radius_auth_failure_rate_high"
)

// Alerter receives operational alerts. Matches fup.Alerter so one
// implementation serves both; declared here rather than imported to avoid
// internal/radius depending on internal/fup.
type Alerter interface {
	Trigger(event string, detail any)
}

// authSample is one reading of a NAS's cumulative counters.
type authSample struct {
	at      time.Time
	accepts float64
	rejects float64
}

// AuthFailureMonitor watches per-NAS authentication outcomes and alerts when
// one NAS starts refusing most of its traffic.
type AuthFailureMonitor struct {
	gatherer prometheus.Gatherer
	alerter  Alerter
	interval time.Duration
	window   time.Duration

	// history holds recent samples per NAS. Bounded by the same registered
	// inventory that bounds the metric's cardinality.
	history map[string][]authSample
	// firing tracks which NAS is already alerting, so a sustained outage
	// produces one alert and a recovery notice rather than one alert per tick.
	firing map[string]bool
}

// NewAuthFailureMonitor constructs the monitor. A nil gatherer uses the
// default registry, which is where promauto registers the counters.
func NewAuthFailureMonitor(alerter Alerter, gatherer prometheus.Gatherer) *AuthFailureMonitor {
	if gatherer == nil {
		gatherer = prometheus.DefaultGatherer
	}
	return &AuthFailureMonitor{
		gatherer: gatherer,
		alerter:  alerter,
		interval: authAlertInterval,
		window:   authAlertWindow,
		history:  map[string][]authSample{},
		firing:   map[string]bool{},
	}
}

// SetInterval overrides the evaluation cadence.
func (m *AuthFailureMonitor) SetInterval(d time.Duration) {
	if d > 0 {
		m.interval = d
	}
}

// SetWindow overrides the evaluation window.
func (m *AuthFailureMonitor) SetWindow(d time.Duration) {
	if d > 0 {
		m.window = d
	}
}

// Run evaluates on an interval until ctx is cancelled.
func (m *AuthFailureMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	// One sample immediately, so the first tick has something to difference
	// against rather than discarding its own reading.
	m.CheckOnce(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.CheckOnce(now)
		}
	}
}

// CheckOnce takes one reading and alerts on any NAS over the threshold.
//
// Rates are computed from the *difference* between the oldest sample still
// inside the window and the newest. The counters are cumulative and never
// reset, so using their absolute values would average over the process's
// whole lifetime — a NAS that failed everything for an hour last week would
// keep the alert firing forever, and one failing right now would be diluted
// to nothing by a week of healthy traffic.
func (m *AuthFailureMonitor) CheckOnce(now time.Time) {
	current, err := m.sample()
	if err != nil {
		log.Warn().Err(err).Msg("radius: could not read auth outcome metrics")
		return
	}

	for nasIP, s := range current {
		s.at = now
		m.history[nasIP] = append(m.history[nasIP], s)
		m.history[nasIP] = trimWindow(m.history[nasIP], now, m.window)

		samples := m.history[nasIP]
		if len(samples) < 2 {
			continue
		}
		oldest, newest := samples[0], samples[len(samples)-1]
		accepts := newest.accepts - oldest.accepts
		rejects := newest.rejects - oldest.rejects
		attempts := accepts + rejects
		if attempts < authAlertMinAttempts {
			// Too quiet for the rate to mean anything. Any alert already
			// firing is cleared: a NAS that has gone silent is a different
			// problem, and continuing to report a failure rate computed from
			// three packets would be inventing one.
			m.clear(nasIP, attempts)
			continue
		}

		rate := rejects / attempts
		if rate <= AuthFailureThreshold {
			m.clear(nasIP, attempts)
			continue
		}
		if m.firing[nasIP] {
			continue // already reported; do not re-alert every tick
		}
		m.firing[nasIP] = true
		m.alerter.Trigger(AuthFailureAlertEvent, map[string]any{
			"nas":          nasIP,
			"failure_rate": rate,
			"threshold":    AuthFailureThreshold,
			"window":       m.window.String(),
			"rejects":      rejects,
			"attempts":     attempts,
		})
		log.Error().Str("nas", nasIP).Float64("failure_rate", rate).
			Float64("attempts", attempts).
			Msg("radius: authentication failure rate above threshold")
	}
}

// clear resolves an alert that is no longer true.
//
// Reported rather than dropped silently, because an operator who saw the alert
// needs to know it stopped — an alert that only ever fires teaches people to
// go and check by hand.
func (m *AuthFailureMonitor) clear(nasIP string, attempts float64) {
	if !m.firing[nasIP] {
		return
	}
	m.firing[nasIP] = false
	m.alerter.Trigger(AuthFailureAlertEvent+"_resolved", map[string]any{
		"nas": nasIP, "attempts": attempts,
	})
	log.Info().Str("nas", nasIP).Msg("radius: authentication failure rate back below threshold")
}

// sample reads the current per-NAS counters out of the registry.
func (m *AuthFailureMonitor) sample() (map[string]authSample, error) {
	families, err := m.gatherer.Gather()
	if err != nil {
		return nil, err
	}

	out := map[string]authSample{}
	for _, family := range families {
		if family.GetName() != "radius_auth_outcome_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			var nasIP, result string
			for _, label := range metric.GetLabel() {
				switch label.GetName() {
				case "nas":
					nasIP = label.GetValue()
				case "result":
					result = label.GetValue()
				}
			}
			if nasIP == "" {
				continue
			}
			s := out[nasIP]
			switch result {
			case "accept":
				s.accepts += metric.GetCounter().GetValue()
			case "reject":
				s.rejects += metric.GetCounter().GetValue()
			}
			out[nasIP] = s
		}
	}
	return out, nil
}

// trimWindow drops samples older than the window, always keeping at least the
// two needed to compute a difference.
func trimWindow(samples []authSample, now time.Time, window time.Duration) []authSample {
	cutoff := now.Add(-window)
	first := 0
	for i, s := range samples {
		if s.at.After(cutoff) {
			first = i
			break
		}
		first = i
	}
	if first > 0 {
		samples = samples[first:]
	}
	// Bound the slice regardless, so a long-lived process with a short window
	// cannot grow it without limit.
	const maxSamples = 64
	if len(samples) > maxSamples {
		samples = samples[len(samples)-maxSamples:]
	}
	return samples
}
