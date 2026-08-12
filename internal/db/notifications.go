package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// NotificationStore persists notification dispatch and delivery state.
// Satisfies notifications.NotifQuerier.
type NotificationStore struct{ pool dbPool }

var _ notifications.NotifQuerier = (*NotificationStore)(nil)

// GetSubscriber loads the DND flag and destination number the dispatcher needs.
func (s *NotificationStore) GetSubscriber(ctx context.Context, subscriberID int) (*notifications.Subscriber, error) {
	const q = `SELECT id, mobile_number, dnd_opt_out FROM subscribers WHERE id = $1`

	var sub notifications.Subscriber
	err := s.pool.QueryRow(ctx, q, subscriberID).Scan(&sub.ID, &sub.MobileNumber, &sub.DndOptOut)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: get subscriber %d for notification: %w", subscriberID, err)
	}
	return &sub, nil
}

// CreateNotificationLog records a dispatch attempt, including a DND suppression.
//
// template_id is nullable and carries an FK to notification_templates: an
// unregistered template is stored as NULL rather than failing the insert, since
// losing the audit row would be worse than losing the template attribution.
func (s *NotificationStore) CreateNotificationLog(ctx context.Context, entry notifications.NotificationLog) error {
	const q = `
		INSERT INTO notification_log (
			subscriber_id, channel, template_id, triggered_by_event,
			sent_at, delivery_status, provider_message_id
		) VALUES (
			$1, $2,
			(SELECT id FROM notification_templates WHERE id = $3),
			$4, COALESCE($5, NOW()), $6, NULLIF($7,'')
		)`

	status := entry.DeliveryStatus
	if status == "" {
		status = "sent"
	}
	var sentAt any
	if !entry.SentAt.IsZero() {
		sentAt = entry.SentAt
	}

	if _, err := s.pool.Exec(ctx, q,
		entry.SubscriberID, entry.Channel, entry.TemplateID, entry.TriggeredByEvent,
		sentAt, status, entry.ProviderMessageID,
	); err != nil {
		return fmt.Errorf("db: create notification log for subscriber %d: %w", entry.SubscriberID, err)
	}
	return nil
}

// UpdateDeliveryStatus advances a logged notification to the status reported by
// the provider's delivery callback.
//
// Meta can deliver callbacks out of order, so a status is only allowed to move
// forward: a late 'sent' must not overwrite a 'read' that already arrived.
func (s *NotificationStore) UpdateDeliveryStatus(ctx context.Context, providerMessageID, status string) error {
	if providerMessageID == "" {
		return fmt.Errorf("db: update delivery status: empty provider_message_id")
	}
	const q = `
		UPDATE notification_log
		SET delivery_status = $2
		WHERE provider_message_id = $1
		  AND CASE $2
		        WHEN 'sent'      THEN 0
		        WHEN 'delivered' THEN 1
		        WHEN 'read'      THEN 2
		        WHEN 'failed'    THEN 3
		        ELSE 0
		      END
		      >
		      CASE delivery_status
		        WHEN 'sent'      THEN 0
		        WHEN 'delivered' THEN 1
		        WHEN 'read'      THEN 2
		        WHEN 'failed'    THEN 3
		        ELSE 0
		      END`

	if _, err := s.pool.Exec(ctx, q, providerMessageID, status); err != nil {
		return fmt.Errorf("db: update delivery status for %s: %w", providerMessageID, err)
	}
	// A zero-row result means the callback was a duplicate or arrived out of
	// order. Both are normal for Meta's at-least-once delivery, so neither is
	// an error the webhook should retry on.
	return nil
}
