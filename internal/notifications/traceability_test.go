package notifications_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// FR-NOTIF-010 requires every WhatsApp message to go out under a template
// pre-approved with Meta. Meta addresses templates by name, not by the
// internal TMPL-00N id, so the mapping is the control: a wrong or missing
// name is silently rejected by Meta at send time rather than caught here.

func TestFR_NOTIF_010_TemplateNameFor_ResolvesRegisteredTemplates(t *testing.T) {
	// The ids seeded into notification_templates, and the Meta names each must
	// resolve to. Written out rather than derived from the map under test —
	// comparing the map to itself would pass whatever it contained.
	want := map[string]string{
		"TMPL-001": "fup_warning_80pct",
		"TMPL-002": "fup_throttled",
		"TMPL-003": "payment_reminder",
		"TMPL-004": "service_suspended",
		"TMPL-005": "payment_received",
		"TMPL-006": "plan_expiring",
		"TMPL-007": "promotional_offer",
	}
	for id, name := range want {
		if got := notifications.TemplateNameFor(id); got != name {
			t.Errorf("TemplateNameFor(%s): want %q, got %q", id, name, got)
		}
	}
}

// TestFR_NOTIF_010_TemplateNameFor_UnknownIDPassesThrough pins the deliberate
// fall-through: a template registered with Meta after this binary shipped must
// still send rather than being blocked by a stale local map.
func TestFR_NOTIF_010_TemplateNameFor_UnknownIDPassesThrough(t *testing.T) {
	if got := notifications.TemplateNameFor("TMPL-999"); got != "TMPL-999" {
		t.Errorf("unknown id should pass through unchanged, got %q", got)
	}
}

// TestFR_NOTIF_010_SendTemplate_UsesApprovedNameOnTheWire is the end of that
// chain: whatever the mapping says must be what actually reaches Meta. A
// correct map paired with a send path that ignored it would still fail in
// production, and only inspecting the request body catches that.
func TestFR_NOTIF_010_SendTemplate_UsesApprovedNameOnTheWire(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		body = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.TMPL"}]}`))
	}))
	defer srv.Close()

	db := &stubNotifDB{subscriber: &notifications.Subscriber{ID: 1, MobileNumber: "+919876543210"}}
	c := notifications.NewWhatsAppClient("phone-id", "token", db)
	c.SetBaseURL(srv.URL)

	err := c.SendTemplate(context.Background(), notifications.TemplateMessage{
		SubscriberID: 1,
		ToPhoneE164:  "+919876543210",
		TemplateName: notifications.TemplateNameFor("TMPL-001"),
		TemplateID:   "TMPL-001",
		TriggerEvent: "fup_warning_80pct",
		Variables:    []string{"sub1", "80%"},
	})
	if err != nil {
		t.Fatalf("SendTemplate: %v", err)
	}
	if !strings.Contains(body, `"type":"template"`) {
		t.Error(`request must declare "type":"template" — free-form messages are not permitted to unopted numbers`)
	}
	if !strings.Contains(body, "fup_warning_80pct") {
		t.Errorf("approved template name missing from the request body: %s", body)
	}
	if strings.Contains(body, "TMPL-001") {
		t.Error("the internal template id leaked to Meta; it expects the approved name")
	}
}
