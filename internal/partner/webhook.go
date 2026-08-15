package partner

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Outbound webhooks — FR-API-002..003 | MDS §4.22.

// Event types partners may subscribe to. A closed set, checked at
// registration: a partner subscribing to "subscriber.suspended" (which does
// not exist) would otherwise register successfully and then wonder for weeks
// why nothing arrived.
const (
	EventSubscriberCreated       = "subscriber.created"
	EventSubscriberStatusChanged = "subscriber.status_changed"
	EventPaymentReceived         = "payment.received"
	EventInvoiceGenerated        = "invoice.generated"
	EventTicketCreated           = "ticket.created"
	EventTicketResolved          = "ticket.resolved"
)

// ValidEvents is the closed set of subscribable events.
var ValidEvents = map[string]bool{
	EventSubscriberCreated:       true,
	EventSubscriberStatusChanged: true,
	EventPaymentReceived:         true,
	EventInvoiceGenerated:        true,
	EventTicketCreated:           true,
	EventTicketResolved:          true,
}

// Signature headers. Names are stable API surface — a partner's verification
// code breaks if these change, so they are constants rather than inline
// strings scattered through the sender.
const (
	HeaderSignature = "X-ISP-Signature"
	HeaderEventID   = "X-ISP-Event-Id"
	HeaderEventType = "X-ISP-Event-Type"
	HeaderTimestamp = "X-ISP-Timestamp"
)

// Event is the thin payload a partner receives.
//
// Thin by decision (2026-08-15): identifiers and a timestamp, never the
// subscriber record. Two reasons, and the second is the load-bearing one.
// A fat payload would put PII in webhook_deliveries, where it would sit under
// DPDP retention rules in what is otherwise a pure audit log. And a payload
// captured at enqueue time is stale by the time it is delivered — a partner
// that re-reads through the API with its own key always sees the current
// truth, and cannot act on a plan change that was superseded while the
// delivery was retrying.
type Event struct {
	EventID    uuid.UUID `json:"event_id"`
	EventType  string    `json:"event_type"`
	EntityID   int       `json:"entity_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

// NewEvent mints an event with a fresh idempotency key.
func NewEvent(eventType string, entityID int, occurredAt time.Time) (Event, error) {
	if !ValidEvents[eventType] {
		return Event{}, fmt.Errorf("partner: unknown event type %q", eventType)
	}
	return Event{
		EventID:    uuid.New(),
		EventType:  eventType,
		EntityID:   entityID,
		OccurredAt: occurredAt.UTC(),
	}, nil
}

// ValidateEvents rejects unknown or empty event subscriptions.
func ValidateEvents(events []string) error {
	if len(events) == 0 {
		return fmt.Errorf("partner: an endpoint needs at least one event")
	}
	for _, e := range events {
		if !ValidEvents[e] {
			return fmt.Errorf("partner: unknown event type %q", e)
		}
	}
	return nil
}

// Sign computes the HMAC-SHA256 signature for a delivery.
//
// The timestamp is inside the signed material, not merely alongside it. Signing
// the body alone would let an attacker who captured one delivery replay it
// indefinitely with a fresh timestamp header; binding the two means a replay
// has to reuse the original timestamp, which a partner checking freshness will
// reject.
//
// Format is "t=<unix>,v1=<hex>" so the scheme can be revised later without
// breaking partners who parse it — a v2 can appear beside v1 during a
// migration rather than replacing it in one cutover.
func Sign(secret string, timestamp time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	ts := strconv.FormatInt(timestamp.Unix(), 10)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks a signature the way a partner would.
//
// Exported because it is the reference implementation: partners are given this
// logic in the integration docs, and having the server test itself against the
// same function is what keeps documentation and behaviour from drifting apart.
func VerifySignature(secret, header string, body []byte, now time.Time, tolerance time.Duration) bool {
	var ts, sig string
	for _, part := range splitComma(header) {
		switch {
		case len(part) > 2 && part[:2] == "t=":
			ts = part[2:]
		case len(part) > 3 && part[:3] == "v1=":
			sig = part[3:]
		}
	}
	if ts == "" || sig == "" {
		return false
	}

	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	// Freshness: a signature valid forever is a replay waiting to happen.
	age := now.Sub(time.Unix(unix, 0))
	if age < -tolerance || age > tolerance {
		return false
	}

	expected := Sign(secret, time.Unix(unix, 0), body)
	return hmac.Equal([]byte(expected), []byte(header)) && sig != ""
}

// splitComma splits on commas without pulling in strings for one call.
func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
