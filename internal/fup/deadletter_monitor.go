package fup

import (
	"context"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

var (
	deadLetterQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fup_dead_letter_queue_depth",
		Help: "Number of tasks in the dead-letter queue",
	})
)

// Alerter is the interface for PagerDuty / alerting integrations.
type Alerter interface {
	Trigger(event string, detail any)
}

// DeadLetterMonitor polls the Asynq dead-letter queue every 30s
// and fires an alert if dead tasks are present.
//
// FR: FR-FUP-003 | DDS §5.3
type DeadLetterMonitor struct {
	redisOpt asynq.RedisConnOpt
	alerter  Alerter
	interval time.Duration
}

// DefaultDeadLetterInterval is how often the archived queue is polled.
const DefaultDeadLetterInterval = 30 * time.Second

// NewDeadLetterMonitor constructs a DeadLetterMonitor.
func NewDeadLetterMonitor(redisOpt asynq.RedisConnOpt, alerter Alerter) *DeadLetterMonitor {
	return &DeadLetterMonitor{redisOpt: redisOpt, alerter: alerter, interval: DefaultDeadLetterInterval}
}

// SetInterval overrides the poll interval.
func (m *DeadLetterMonitor) SetInterval(d time.Duration) {
	m.interval = d
}

// Run starts the monitoring loop. Blocks until ctx is cancelled.
func (m *DeadLetterMonitor) Run(ctx context.Context) {
	interval := m.interval
	if interval <= 0 {
		interval = DefaultDeadLetterInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	inspector := asynq.NewInspector(m.redisOpt)
	defer inspector.Close() //nolint:errcheck

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.checkOnce(inspector); err != nil {
				log.Error().Err(err).Msg("dead_letter_monitor: queue info error")
			}
		}
	}
}

// checkOnce samples the archived depth and alerts if any task has dead-lettered.
func (m *DeadLetterMonitor) checkOnce(inspector *asynq.Inspector) error {
	info, err := inspector.GetQueueInfo(QueueNetCommands)
	if err != nil {
		return fmt.Errorf("dead_letter_monitor: get queue info: %w", err)
	}
	deadLetterQueueDepth.Set(float64(info.Archived))
	if info.Archived > 0 {
		log.Warn().Int("archived_count", info.Archived).Msg("dead_letter_monitor: archived tasks detected")
		m.alerter.Trigger("dead_letter_queue_non_empty", info.Archived)
	}
	return nil
}
