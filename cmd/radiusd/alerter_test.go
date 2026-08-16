package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// logAlerter is shared by four monitors — dead-letter, revenue reconciliation,
// SLA breach, and per-NAS auth failure — and a structured log line alone
// requires something tailing this process's stdout to ever see it. This test
// pins the other half: an existing Prometheus scrape catches the same event.
func TestLogAlerterIncrementsTheScrapedCounter(t *testing.T) {
	before := counterValue(t, "dead_letter_queue_non_empty")

	logAlerter{}.Trigger("dead_letter_queue_non_empty", map[string]any{"count": 3})

	if got := counterValue(t, "dead_letter_queue_non_empty"); got != before+1 {
		t.Errorf("alerts_emitted_total{alert_name=%q}: want +1, got %v",
			"dead_letter_queue_non_empty", got-before)
	}
}

// TestLogAlerterLabelsBySpecificEvent — a scraping pipeline needs to
// distinguish alert types, so two different events must not share one series.
func TestLogAlerterLabelsBySpecificEvent(t *testing.T) {
	beforeA := counterValue(t, "ledger_variance_detected")
	beforeB := counterValue(t, "radius_auth_failure_rate_high")

	logAlerter{}.Trigger("ledger_variance_detected", nil)

	if got := counterValue(t, "ledger_variance_detected"); got != beforeA+1 {
		t.Errorf("ledger_variance_detected: want +1, got %v", got-beforeA)
	}
	if got := counterValue(t, "radius_auth_failure_rate_high"); got != beforeB {
		t.Errorf("an unrelated alert name must not move, got +%v", got-beforeB)
	}
}

func counterValue(t *testing.T, alertName string) float64 {
	t.Helper()
	c, err := alertsEmitted.GetMetricWithLabelValues(alertName)
	if err != nil {
		t.Fatalf("counter for %q: %v", alertName, err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

var _ prometheus.Collector = alertsEmitted
