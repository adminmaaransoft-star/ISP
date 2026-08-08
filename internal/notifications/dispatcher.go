package notifications

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
)

// Dispatcher routes notification tasks to the correct channel client
// after applying the DND suppression check (FR-NOTIF-008).
//
// FR: FR-NOTIF-001..011 | DDS §5.8
type Dispatcher struct {
	db        NotifQuerier
	whatsapp  *WhatsAppClient
	smsClient SMSSender
}

// SMSSender is the interface for SMS gateway implementations.
type SMSSender interface {
	SendSMS(ctx context.Context, toPhone, message string) error
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(db NotifQuerier, wa *WhatsAppClient, sms SMSSender) *Dispatcher {
	return &Dispatcher{db: db, whatsapp: wa, smsClient: sms}
}

// Dispatch applies DND suppression, then routes to the correct channel.
func (d *Dispatcher) Dispatch(ctx context.Context, task NotificationTask) error {
	sub, err := d.db.GetSubscriber(ctx, task.SubscriberID)
	if err != nil {
		return fmt.Errorf("notifications: get subscriber %d: %w", task.SubscriberID, err)
	}

	// DND suppression: only suppress marketing-class notifications (FR-NOTIF-008)
	if sub.DndOptOut && task.Class == "marketing" {
		log.Info().
			Int("subscriber_id", task.SubscriberID).
			Str("event", task.TriggerEvent).
			Msg("notification suppressed: DND opt-out")

		_ = d.db.CreateNotificationLog(ctx, NotificationLog{
			SubscriberID:     task.SubscriberID,
			Channel:          task.Channel,
			TemplateID:       task.TemplateID,
			TriggeredByEvent: task.TriggerEvent,
			DeliveryStatus:   "suppressed_dnd",
		})
		return nil // intentional suppression, not an error
	}

	switch task.Channel {
	case "whatsapp":
		if d.whatsapp == nil {
			return fmt.Errorf("notifications: WhatsApp client not configured")
		}
		toPhone := task.ToPhone
		if toPhone == "" {
			toPhone = sub.MobileNumber
		}
		return d.whatsapp.SendTemplate(ctx, TemplateMessage{
			SubscriberID: task.SubscriberID,
			ToPhoneE164:  toPhone,
			TemplateName: TemplateNameFor(task.TemplateID),
			TemplateID:   task.TemplateID,
			TriggerEvent: task.TriggerEvent,
			Variables:    task.Variables,
		})
	case "sms":
		if d.smsClient == nil {
			return fmt.Errorf("notifications: SMS client not configured")
		}
		return d.smsClient.SendSMS(ctx, task.ToPhone, task.Variables[0])
	default:
		return fmt.Errorf("notifications: unsupported channel %q", task.Channel)
	}
}

// Notify sends a transactional WhatsApp template to a subscriber, resolving the
// destination number from the subscriber record.
//
// It is the entry point used by Asynq task handlers, which know a subscriber ID
// and a template but not a phone number.
func (d *Dispatcher) Notify(ctx context.Context, subscriberID int, templateID, triggerEvent string, vars []string) error {
	return d.Dispatch(ctx, NotificationTask{
		SubscriberID: subscriberID,
		Channel:      "whatsapp",
		TemplateID:   templateID,
		TriggerEvent: triggerEvent,
		Class:        "transactional",
		Variables:    vars,
	})
}
