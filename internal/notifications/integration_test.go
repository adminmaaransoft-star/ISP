//go:build integration

// Integration tests for the notification dispatcher, WhatsApp sender and the
// Meta delivery-status webhook.
//
// Covers INT-NOTIF-001 .. INT-NOTIF-004 from the Integration Tests tracker sheet.
// The Meta Cloud API is replaced by an httptest server, so the real HTTP request
// construction, response decoding and notification_log writes are exercised.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/notifications -Tags integration
package notifications_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// ── In-memory notification_log ──────────────────────────────────────────────

// itNotifStore models the notification_log table and the subscriber row the
// dispatcher reads its DND flag from.
type itNotifStore struct {
	mu         sync.Mutex
	subscriber *notifications.Subscriber
	rows       []notifications.NotificationLog
}

func (s *itNotifStore) GetSubscriber(context.Context, int) (*notifications.Subscriber, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscriber, nil
}

func (s *itNotifStore) CreateNotificationLog(_ context.Context, entry notifications.NotificationLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, entry)
	return nil
}

func (s *itNotifStore) UpdateDeliveryStatus(_ context.Context, providerMessageID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if s.rows[i].ProviderMessageID == providerMessageID {
			s.rows[i].DeliveryStatus = status
		}
	}
	return nil
}

func (s *itNotifStore) snapshot() []notifications.NotificationLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notifications.NotificationLog(nil), s.rows...)
}

// itSMSRecorder counts SMS sends.
type itSMSRecorder struct {
	mu    sync.Mutex
	calls int
}

func (s *itSMSRecorder) SendSMS(context.Context, string, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return nil
}

func (s *itSMSRecorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ── Stub Meta Cloud API ─────────────────────────────────────────────────────

type itMetaServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
}

func (m *itMetaServer) received() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.requests...)
}

// itNewMetaServer returns a stub Meta endpoint that echoes a wamid message ID.
func itNewMetaServer(t *testing.T, messageID string, status int) *itMetaServer {
	t.Helper()
	m := &itMetaServer{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.requests = append(m.requests, body)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messaging_product": "whatsapp",
			"messages":          []map[string]string{{"id": messageID}},
		})
	}))
	t.Cleanup(m.Close)
	return m
}

func itMetaSignature(body []byte, appSecret string) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// itDeliveryCallback builds a Meta delivery-status webhook body.
func itDeliveryCallback(messageID, status string) []byte {
	b, _ := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []map[string]any{{
			"id": "WABA-1",
			"changes": []map[string]any{{
				"field": "messages",
				"value": map[string]any{
					"messaging_product": "whatsapp",
					"statuses": []map[string]any{{
						"id":           messageID,
						"status":       status,
						"timestamp":    "1717171717",
						"recipient_id": "919876543210",
					}},
				},
			}},
		}},
	})
	return b
}

// ── INT-NOTIF-001 ───────────────────────────────────────────────────────────

// TestDispatcher_DND_SuppressesMarketing verifies a DND subscriber receives no
// marketing message and that the suppression is recorded in notification_log.
//
// INT-NOTIF-001 | FR-NOTIF-008
func TestDispatcher_DND_SuppressesMarketing(t *testing.T) {
	meta := itNewMetaServer(t, "wamid.should_not_be_used", http.StatusOK)
	store := &itNotifStore{subscriber: &notifications.Subscriber{
		ID: 1, MobileNumber: "+919876543210", DndOptOut: true,
	}}

	wa := notifications.NewWhatsAppClient("phone-id", "token", store)
	wa.SetBaseURL(meta.URL)
	sms := &itSMSRecorder{}
	dispatcher := notifications.NewDispatcher(store, wa, sms)

	err := dispatcher.Dispatch(context.Background(), notifications.NotificationTask{
		SubscriberID: 1,
		Channel:      "whatsapp",
		TemplateID:   "TMPL-007",
		TriggerEvent: "promotional_campaign",
		Class:        "marketing",
		ToPhone:      "+919876543210",
		Variables:    []string{"50% off"},
	})
	if err != nil {
		t.Fatalf("suppression must not be an error, got: %v", err)
	}

	if got := meta.received(); len(got) != 0 {
		t.Errorf("no Meta API call expected for a DND subscriber, got %d", len(got))
	}
	if sms.count() != 0 {
		t.Errorf("no SMS expected for a DND subscriber, got %d", sms.count())
	}

	rows := store.snapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 notification_log row, got %d", len(rows))
	}
	if rows[0].DeliveryStatus != "suppressed_dnd" {
		t.Errorf("delivery_status: want suppressed_dnd, got %q", rows[0].DeliveryStatus)
	}
	if rows[0].TemplateID != "TMPL-007" {
		t.Errorf("template_id: want TMPL-007, got %q", rows[0].TemplateID)
	}
}

// TestDispatcher_DND_AllowsTransactional verifies service-critical messages are
// still delivered to a DND subscriber.
//
// INT-NOTIF-001 (supporting) | FR-NOTIF-008
func TestDispatcher_DND_AllowsTransactional(t *testing.T) {
	meta := itNewMetaServer(t, "wamid.transactional", http.StatusOK)
	store := &itNotifStore{subscriber: &notifications.Subscriber{
		ID: 1, MobileNumber: "+919876543210", DndOptOut: true,
	}}

	wa := notifications.NewWhatsAppClient("phone-id", "token", store)
	wa.SetBaseURL(meta.URL)
	dispatcher := notifications.NewDispatcher(store, wa, &itSMSRecorder{})

	err := dispatcher.Dispatch(context.Background(), notifications.NotificationTask{
		SubscriberID: 1,
		Channel:      "whatsapp",
		TemplateID:   "TMPL-004",
		TriggerEvent: "service_suspended",
		Class:        "transactional",
		ToPhone:      "+919876543210",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if got := meta.received(); len(got) != 1 {
		t.Fatalf("transactional message must reach Meta, got %d calls", len(got))
	}
	rows := store.snapshot()
	if len(rows) != 1 || rows[0].DeliveryStatus != "sent" {
		t.Errorf("want one row with delivery_status=sent, got %+v", rows)
	}
}

// ── INT-NOTIF-002 ───────────────────────────────────────────────────────────

// TestWhatsApp_SendTemplate_LogsProviderID verifies the Meta message ID is
// persisted to notification_log against the whatsapp channel.
//
// INT-NOTIF-002 | FR-NOTIF-009
func TestWhatsApp_SendTemplate_LogsProviderID(t *testing.T) {
	const wantID = "wamid.HBgMOTE5ODc2NTQzMjEwFQIAERgSN0"
	meta := itNewMetaServer(t, wantID, http.StatusOK)
	store := &itNotifStore{subscriber: &notifications.Subscriber{ID: 2, MobileNumber: "+919876543210"}}

	wa := notifications.NewWhatsAppClient("phone-id-123", "meta-token", store)
	wa.SetBaseURL(meta.URL)

	err := wa.SendTemplate(context.Background(), notifications.TemplateMessage{
		SubscriberID: 2,
		ToPhoneE164:  "+919876543210",
		TemplateID:   "TMPL-001",
		TriggerEvent: "fup_warning_80pct",
		Variables:    []string{"nearly@isp", "82%"},
	})
	if err != nil {
		t.Fatalf("SendTemplate: %v", err)
	}

	rows := store.snapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 notification_log row, got %d", len(rows))
	}
	if rows[0].ProviderMessageID != wantID {
		t.Errorf("provider_message_id: want %q, got %q", wantID, rows[0].ProviderMessageID)
	}
	if rows[0].Channel != "whatsapp" {
		t.Errorf("channel: want whatsapp, got %q", rows[0].Channel)
	}
	if rows[0].DeliveryStatus != "sent" {
		t.Errorf("delivery_status: want sent, got %q", rows[0].DeliveryStatus)
	}
	if rows[0].SentAt.IsZero() {
		t.Error("sent_at must be populated")
	}

	// The outbound payload must name the Meta template, not our internal ID.
	sent := meta.received()
	if len(sent) != 1 {
		t.Fatalf("want 1 Meta call, got %d", len(sent))
	}
	tmpl, ok := sent[0]["template"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no template object: %+v", sent[0])
	}
	if want := notifications.TemplateNameFor("TMPL-001"); tmpl["name"] != want {
		t.Errorf("template name: want %q, got %v", want, tmpl["name"])
	}
}

// TestWhatsApp_SendTemplate_APIErrorNotLoggedAsSent verifies a rejected send is
// reported as an error rather than recorded as delivered.
//
// INT-NOTIF-002 (supporting) | FR-NOTIF-009
func TestWhatsApp_SendTemplate_APIErrorNotLoggedAsSent(t *testing.T) {
	meta := itNewMetaServer(t, "", http.StatusUnauthorized)
	store := &itNotifStore{subscriber: &notifications.Subscriber{ID: 3}}

	wa := notifications.NewWhatsAppClient("phone-id", "expired-token", store)
	wa.SetBaseURL(meta.URL)

	err := wa.SendTemplate(context.Background(), notifications.TemplateMessage{
		SubscriberID: 3,
		ToPhoneE164:  "+919876543210",
		TemplateID:   "TMPL-001",
	})
	if err == nil {
		t.Fatal("expected an error when Meta rejects the send")
	}
	for _, row := range store.snapshot() {
		if row.DeliveryStatus == "sent" {
			t.Error("a rejected send must not be logged as sent")
		}
	}
}

// ── INT-NOTIF-003 ───────────────────────────────────────────────────────────

// TestWhatsAppWebhook_UpdatesDeliveryStatus verifies a signed Meta callback
// advances the logged notification from sent to delivered.
//
// INT-NOTIF-003 | FR-NOTIF-011
func TestWhatsAppWebhook_UpdatesDeliveryStatus(t *testing.T) {
	const appSecret = "meta_app_secret"
	const messageID = "wamid.delivery_test"

	meta := itNewMetaServer(t, messageID, http.StatusOK)
	store := &itNotifStore{subscriber: &notifications.Subscriber{ID: 4, MobileNumber: "+919876543210"}}

	// Send first so there is a row in state 'sent' to advance.
	wa := notifications.NewWhatsAppClient("phone-id", "token", store)
	wa.SetBaseURL(meta.URL)
	if err := wa.SendTemplate(context.Background(), notifications.TemplateMessage{
		SubscriberID: 4,
		ToPhoneE164:  "+919876543210",
		TemplateID:   "TMPL-001",
	}); err != nil {
		t.Fatalf("SendTemplate: %v", err)
	}
	if rows := store.snapshot(); len(rows) != 1 || rows[0].DeliveryStatus != "sent" {
		t.Fatalf("precondition failed, want one row in state sent, got %+v", rows)
	}

	handler := notifications.NewWebhookHandler(store, appSecret, "verify-token")
	body := itDeliveryCallback(messageID, "delivered")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", itMetaSignature(body, appSecret))
	rec := httptest.NewRecorder()

	handler.HandleDeliveryStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	rows := store.snapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].DeliveryStatus != "delivered" {
		t.Errorf("delivery_status: want delivered, got %q", rows[0].DeliveryStatus)
	}
}

// TestWhatsAppWebhook_StatusProgression verifies successive callbacks advance
// the same row through the delivery lifecycle.
//
// INT-NOTIF-003 (supporting) | FR-NOTIF-011
func TestWhatsAppWebhook_StatusProgression(t *testing.T) {
	const appSecret = "meta_app_secret"
	const messageID = "wamid.progression"

	meta := itNewMetaServer(t, messageID, http.StatusOK)
	store := &itNotifStore{subscriber: &notifications.Subscriber{ID: 5}}
	wa := notifications.NewWhatsAppClient("phone-id", "token", store)
	wa.SetBaseURL(meta.URL)
	if err := wa.SendTemplate(context.Background(), notifications.TemplateMessage{
		SubscriberID: 5, ToPhoneE164: "+919876543210", TemplateID: "TMPL-003",
	}); err != nil {
		t.Fatalf("SendTemplate: %v", err)
	}

	handler := notifications.NewWebhookHandler(store, appSecret, "verify-token")

	for _, status := range []string{"delivered", "read"} {
		body := itDeliveryCallback(messageID, status)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", itMetaSignature(body, appSecret))
		rec := httptest.NewRecorder()

		handler.HandleDeliveryStatus(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status %s: want 200, got %d", status, rec.Code)
		}
		rows := store.snapshot()
		if rows[0].DeliveryStatus != status {
			t.Errorf("after %s callback: got %q", status, rows[0].DeliveryStatus)
		}
	}
}

// ── INT-NOTIF-004 ───────────────────────────────────────────────────────────

// TestWhatsAppWebhook_InvalidHMAC verifies unsigned, wrongly signed and tampered
// callbacks are rejected with 400 and leave notification_log untouched.
//
// INT-NOTIF-004 | FR-NOTIF-011
func TestWhatsAppWebhook_InvalidHMAC(t *testing.T) {
	const appSecret = "meta_app_secret"
	const messageID = "wamid.hmac_test"

	meta := itNewMetaServer(t, messageID, http.StatusOK)
	store := &itNotifStore{subscriber: &notifications.Subscriber{ID: 6}}
	wa := notifications.NewWhatsAppClient("phone-id", "token", store)
	wa.SetBaseURL(meta.URL)
	if err := wa.SendTemplate(context.Background(), notifications.TemplateMessage{
		SubscriberID: 6, ToPhoneE164: "+919876543210", TemplateID: "TMPL-001",
	}); err != nil {
		t.Fatalf("SendTemplate: %v", err)
	}

	handler := notifications.NewWebhookHandler(store, appSecret, "verify-token")
	body := itDeliveryCallback(messageID, "delivered")

	cases := []struct {
		name      string
		signature string
	}{
		{"missing header", ""},
		{"wrong secret", itMetaSignature(body, "attacker_secret")},
		{"missing sha256 prefix", itMetaSignature(body, appSecret)[len("sha256="):]},
		{"truncated digest", itMetaSignature(body, appSecret)[:20]},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewReader(body))
			if c.signature != "" {
				req.Header.Set("X-Hub-Signature-256", c.signature)
			}
			rec := httptest.NewRecorder()

			handler.HandleDeliveryStatus(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d", rec.Code)
			}
		})
	}

	// The row must still be in the state the send left it in.
	rows := store.snapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].DeliveryStatus != "sent" {
		t.Errorf("rejected callbacks must not change delivery_status, got %q", rows[0].DeliveryStatus)
	}
}

// TestWhatsAppWebhook_TamperedBodyRejected verifies a body altered after signing
// fails validation.
//
// INT-NOTIF-004 (supporting) | FR-NOTIF-011
func TestWhatsAppWebhook_TamperedBodyRejected(t *testing.T) {
	const appSecret = "meta_app_secret"
	store := &itNotifStore{subscriber: &notifications.Subscriber{ID: 7}}
	handler := notifications.NewWebhookHandler(store, appSecret, "verify-token")

	signed := itDeliveryCallback("wamid.original", "delivered")
	tampered := itDeliveryCallback("wamid.substituted", "delivered")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewReader(tampered))
	req.Header.Set("X-Hub-Signature-256", itMetaSignature(signed, appSecret))
	rec := httptest.NewRecorder()

	handler.HandleDeliveryStatus(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a body tampered after signing, got %d", rec.Code)
	}
}

// TestWhatsAppWebhook_VerifyHandshake verifies Meta's subscription handshake
// echoes the challenge only when the verify token matches.
//
// INT-NOTIF-004 (supporting) | FR-NOTIF-011
func TestWhatsAppWebhook_VerifyHandshake(t *testing.T) {
	handler := notifications.NewWebhookHandler(&itNotifStore{}, "secret", "the-verify-token")

	t.Run("correct token echoes challenge", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=the-verify-token&hub.challenge=1158201444", nil)
		rec := httptest.NewRecorder()

		handler.Verify(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
		if rec.Body.String() != "1158201444" {
			t.Errorf("challenge echo: want 1158201444, got %q", rec.Body.String())
		}
	})

	t.Run("wrong token refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=guessed&hub.challenge=1158201444", nil)
		rec := httptest.NewRecorder()

		handler.Verify(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("want 403, got %d", rec.Code)
		}
	})

	t.Run("challenge is sanitised", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=the-verify-token&hub.challenge=%3Cscript%3Ealert(1)%3C/script%3E", nil)
		rec := httptest.NewRecorder()

		handler.Verify(rec, req)

		if got := rec.Body.String(); bytes.ContainsAny([]byte(got), "<>()/") {
			t.Errorf("challenge echo must strip markup characters, got %q", got)
		}
	})
}
