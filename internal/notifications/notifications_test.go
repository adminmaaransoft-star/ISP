package notifications_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// stubNotifDB satisfies notifications.NotifQuerier for unit tests.
type stubNotifDB struct {
	subscriber    *notifications.Subscriber
	loggedEntries []notifications.NotificationLog
	statusUpdates map[string]string
	pushTokens    []string
}

func (s *stubNotifDB) ListPushTokens(_ context.Context, _ int) ([]string, error) {
	return s.pushTokens, nil
}

func (s *stubNotifDB) GetSubscriber(_ context.Context, _ int) (*notifications.Subscriber, error) {
	return s.subscriber, nil
}
func (s *stubNotifDB) CreateNotificationLog(_ context.Context, entry notifications.NotificationLog) error {
	s.loggedEntries = append(s.loggedEntries, entry)
	return nil
}
func (s *stubNotifDB) UpdateDeliveryStatus(_ context.Context, providerMessageID, status string) error {
	if s.statusUpdates == nil {
		s.statusUpdates = map[string]string{}
	}
	s.statusUpdates[providerMessageID] = status
	return nil
}

// stubSMS records SendSMS calls.
type stubSMS struct{ calls int }

func (s *stubSMS) SendSMS(_ context.Context, _, _ string) error {
	s.calls++
	return nil
}

// TestDispatch_DND_Suppresses_Marketing verifies that DND subscribers
// do not receive marketing-class notifications.
func TestDispatch_DND_Suppresses_Marketing(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{DndOptOut: true}}
	sms := &stubSMS{}

	dispatcher := notifications.NewDispatcher(db, nil, sms)

	err := dispatcher.Dispatch(context.Background(), notifications.NotificationTask{
		SubscriberID: 1,
		Channel:      "sms",
		TemplateID:   "TMPL-007",
		TriggerEvent: "payment_received",
		Class:        "marketing", // should be suppressed
		ToPhone:      "+919876543210",
		Variables:    []string{"test message"},
	})

	if err != nil {
		t.Fatalf("expected nil error for DND suppression, got %v", err)
	}
	if sms.calls != 0 {
		t.Errorf("expected 0 SMS calls for DND subscriber, got %d", sms.calls)
	}
	if len(db.loggedEntries) == 0 {
		t.Error("expected suppressed_dnd log entry, got none")
	}
	if db.loggedEntries[0].DeliveryStatus != "suppressed_dnd" {
		t.Errorf("expected delivery_status=suppressed_dnd, got %s", db.loggedEntries[0].DeliveryStatus)
	}
}

// TestDispatch_DND_Allows_Transactional verifies that transactional notifications
// bypass DND suppression and reach the SMS client.
func TestDispatch_DND_Allows_Transactional(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{DndOptOut: true}}
	sms := &stubSMS{}

	dispatcher := notifications.NewDispatcher(db, nil, sms)

	err := dispatcher.Dispatch(context.Background(), notifications.NotificationTask{
		SubscriberID: 1,
		Channel:      "sms",
		TemplateID:   "TMPL-004",
		TriggerEvent: "soft_suspension",
		Class:        "transactional", // must NOT be suppressed
		ToPhone:      "+919876543210",
		Variables:    []string{"Your service is suspended."},
	})

	if err != nil {
		t.Fatalf("unexpected error for transactional notification: %v", err)
	}
	if sms.calls != 1 {
		t.Errorf("expected 1 SMS call for transactional, got %d", sms.calls)
	}
}

// TestSMS_InvalidPhoneRejected verifies DoD Phase 2 Step 4: SendSMS refuses
// a non-E.164 phone number before ever attempting the gateway call.
func TestSMS_InvalidPhoneRejected(t *testing.T) {
	client := notifications.NewMSG91Client("test-key", "TESTID")

	err := client.SendSMS(context.Background(), "not-a-phone-number", "hello")
	if err == nil {
		t.Fatal("expected an error for a non-E.164 phone number, got nil")
	}
}

// TestWhatsApp_SendTemplate_InvalidPhoneRejected is the WhatsApp-channel
// counterpart: SendTemplate must refuse a non-E.164 phone number before
// ever calling the Meta API — proven here by asserting the mock server
// never received a request, not just that an error came back.
func TestWhatsApp_SendTemplate_InvalidPhoneRejected(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wa := notifications.NewWhatsAppClient("phone-id", "token", &stubNotifDB{})
	wa.SetBaseURL(srv.URL)

	err := wa.SendTemplate(context.Background(), notifications.TemplateMessage{
		SubscriberID: 1,
		ToPhoneE164:  "not-a-phone-number",
		TemplateID:   "TMPL-001",
	})
	if err == nil {
		t.Fatal("expected an error for a non-E.164 phone number, got nil")
	}
	if called {
		t.Error("the Meta API must not be called when the phone number is invalid")
	}
}
