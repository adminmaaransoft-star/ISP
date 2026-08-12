package tickets

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type stubSLAQuerier struct {
	events []SLAEvent
	err    error
	calls  int
	// lastFraction records what the scanner asked for, so the 80% convention
	// is asserted rather than assumed.
	lastFraction float64
}

func (s *stubSLAQuerier) ClaimSLAEvents(_ context.Context, warningFraction float64) ([]SLAEvent, error) {
	s.calls++
	s.lastFraction = warningFraction
	return s.events, s.err
}

type recordingAlerter struct {
	mu     sync.Mutex
	events []string
	detail []any
}

func (a *recordingAlerter) Trigger(event string, detail any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	a.detail = append(a.detail, detail)
}

func (a *recordingAlerter) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.events...)
}

func strptr(s string) *string { return &s }

// TestScan_BreachesAlert_WarningsDoNot is the core routing rule: a ticket at
// 80% of its window is information, a passed deadline is an incident. If
// both fired the alerter, staff would learn to ignore the channel.
func TestScan_BreachesAlert_WarningsDoNot(t *testing.T) {
	db := &stubSLAQuerier{events: []SLAEvent{
		{TicketID: 1, EventType: "response_warning", SubscriberID: 10, Priority: "high", RoutedRole: strptr("csr")},
		{TicketID: 2, EventType: "response_breach", SubscriberID: 11, Priority: "critical", RoutedRole: strptr("technician")},
		{TicketID: 3, EventType: "resolution_warning", SubscriberID: 12, Priority: "low"},
		{TicketID: 4, EventType: "resolution_breach", SubscriberID: 13, Priority: "medium", RoutedRole: strptr("csr")},
	}}
	alerter := &recordingAlerter{}

	if err := NewSLAScanner(db, alerter).Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	got := alerter.snapshot()
	want := []string{"response_breach", "resolution_breach"}
	if len(got) != len(want) {
		t.Fatalf("alerter fired %d times (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("alert[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestScan_UsesTheEightyPercentWarningThreshold(t *testing.T) {
	db := &stubSLAQuerier{}
	if err := NewSLAScanner(db, &recordingAlerter{}).Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if db.lastFraction != 0.8 {
		t.Errorf("warning fraction = %v, want 0.8 (the same threshold FR-FUP-004 uses)", db.lastFraction)
	}
}

func TestScan_AlertDetailCarriesEnoughToActOn(t *testing.T) {
	db := &stubSLAQuerier{events: []SLAEvent{
		{TicketID: 42, EventType: "resolution_breach", SubscriberID: 7, Priority: "critical", RoutedRole: strptr("technician")},
	}}
	alerter := &recordingAlerter{}

	if err := NewSLAScanner(db, alerter).Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	detail, ok := alerter.detail[0].(map[string]any)
	if !ok {
		t.Fatalf("alert detail is %T, want map[string]any", alerter.detail[0])
	}
	// An alert naming only "a ticket breached" would send someone hunting.
	for k, want := range map[string]any{
		"ticket_id": 42, "subscriber_id": 7, "priority": "critical", "routed_role": "technician",
	} {
		if detail[k] != want {
			t.Errorf("detail[%q] = %v, want %v", k, detail[k], want)
		}
	}
}

func TestScan_UnroutedTicketDoesNotPanic(t *testing.T) {
	// RoutedRole is nil whenever no routing rule matched, which migration
	// 023 makes a normal outcome rather than an error — dereferencing it
	// blindly would turn an ordinary unrouted ticket into a crashed scanner.
	db := &stubSLAQuerier{events: []SLAEvent{
		{TicketID: 5, EventType: "response_breach", SubscriberID: 14, Priority: "medium", RoutedRole: nil},
	}}
	alerter := &recordingAlerter{}

	if err := NewSLAScanner(db, alerter).Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	detail := alerter.detail[0].(map[string]any)
	if detail["routed_role"] != "unrouted" {
		t.Errorf("routed_role = %v, want %q", detail["routed_role"], "unrouted")
	}
}

func TestScan_QueryErrorPropagates(t *testing.T) {
	wantErr := errors.New("database is down")
	db := &stubSLAQuerier{err: wantErr}

	err := NewSLAScanner(db, &recordingAlerter{}).Scan(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Scan error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestScan_NilAlerterIsTolerated(t *testing.T) {
	// Every other optional dependency in this codebase degrades rather than
	// panicking when unset; the scanner should not be the exception.
	db := &stubSLAQuerier{events: []SLAEvent{
		{TicketID: 6, EventType: "response_breach", SubscriberID: 15, Priority: "high"},
	}}

	if err := NewSLAScanner(db, nil).Scan(context.Background()); err != nil {
		t.Fatalf("Scan with nil alerter: %v", err)
	}
}

func TestIsBreach(t *testing.T) {
	cases := map[string]bool{
		"response_breach":    true,
		"resolution_breach":  true,
		"response_warning":   false,
		"resolution_warning": false,
	}
	for eventType, want := range cases {
		if got := (SLAEvent{EventType: eventType}).IsBreach(); got != want {
			t.Errorf("IsBreach(%q) = %v, want %v", eventType, got, want)
		}
	}
}
