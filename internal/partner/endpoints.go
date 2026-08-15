package partner

import (
	"time"

	"github.com/google/uuid"
)

// Webhook endpoint and delivery records — FR-API-002..003 | MDS §4.22.

// WebhookEndpoint is a partner callback as shown to an operator. The signing
// secret is absent on purpose: it is returned once at registration and after
// that exists only in encrypted form, because an endpoint listing is a routine
// read and should not hand back a credential.
type WebhookEndpoint struct {
	ID          int       `json:"id"`
	APIKeyID    int       `json:"api_key_id"`
	URL         string    `json:"url"`
	Events      []string  `json:"events"`
	Active      bool      `json:"active"`
	Description *string   `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// EndpointSecret is what the sender needs: where to post, and the still-
// encrypted secret to sign with.
type EndpointSecret struct {
	EndpointID      int
	URL             string
	SecretEncrypted string
	KeyVersion      string
}

// Delivery is one attempt trail, the audit record FR-API-003 requires.
type Delivery struct {
	ID              int64      `json:"id"`
	EndpointID      int        `json:"endpoint_id"`
	EventID         uuid.UUID  `json:"event_id"`
	EventType       string     `json:"event_type"`
	Status          string     `json:"status"`
	Attempts        int        `json:"attempts"`
	ResponseStatus  *int       `json:"response_status,omitempty"`
	ResponseExcerpt *string    `json:"response_excerpt,omitempty"`
	LastError       *string    `json:"last_error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	DeliveredAt     *time.Time `json:"delivered_at,omitempty"`
}

// Delivery statuses, matching webhook_deliveries.status.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusFailed    = "failed"
	// StatusAbandoned is distinct from failed on purpose: failed is one bad
	// attempt, abandoned means retries are exhausted and nobody will try
	// again. Collapsing them hides the state that actually needs a human.
	StatusAbandoned = "abandoned"
)
