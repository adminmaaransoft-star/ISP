package fup

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"layeh.com/radius"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

var podAckTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "fup_pod_ack_total",
	Help: "PoD (Disconnect-Request) response counts by result",
}, []string{"result"})

// PoDPayload is the Asynq task payload for a forced disconnect.
type PoDPayload struct {
	SubscriberID int `json:"subscriber_id"`
}

// PoDHandler processes forced-disconnect tasks with exponential backoff via
// Asynq retry, sharing CoAQuerier since both need the same NAS session lookup.
//
// FR: FR-NET-002, FR-NAS-002 | DDS §5.3 | MDS §4.11
type PoDHandler struct {
	db          CoAQuerier
	secret      []byte
	port        int
	nasResolver *nas.Resolver
}

// NewPoDHandler constructs a PoDHandler targeting DefaultCoAPort.
func NewPoDHandler(db CoAQuerier, secret []byte) *PoDHandler {
	return &PoDHandler{db: db, secret: secret, port: DefaultCoAPort}
}

// SetPort overrides the NAS PoD destination port.
func (h *PoDHandler) SetPort(port int) {
	h.port = port
}

// SetNASResolver enables per-NAS secret/port resolution (FR-NAS-002, MDS
// §4.11). PoD carries no vendor-specific attribute for any vendor, so
// unlike CoAHandler this only affects which secret/port is used, never
// packet contents. Optional: unset behaves exactly as before.
func (h *PoDHandler) SetNASResolver(r *nas.Resolver) {
	h.nasResolver = r
}

// ProcessTask implements asynq.Handler.
func (h *PoDHandler) ProcessTask(ctx context.Context, t *asynq.Task) error {
	var p PoDPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("pod: unmarshal payload: %w", err)
	}

	nasIP, sessionID, _, _, err := h.db.GetSubscriberNASSession(ctx, p.SubscriberID)
	if err != nil {
		// No live session means there is nothing left to disconnect. Retrying
		// cannot change that, so the task should not be retried.
		return fmt.Errorf("pod: get NAS session for sub %d: %w: %w", p.SubscriberID, err, asynq.SkipRetry)
	}

	secret, port := h.secret, h.port
	if port == 0 {
		port = DefaultCoAPort
	}
	if h.nasResolver != nil {
		device := h.nasResolver.Resolve(nasIP)
		secret, port = device.Secret, device.PoDPort
	}
	return SendReliablePoD(nasIP, port, sessionID, secret)
}

// SendReliablePoD sends a Disconnect-Request to the NAS and waits for
// Disconnect-ACK. Returns an error on NAK or timeout — Asynq will retry with
// exponential backoff.
//
// DDS §5.3
func SendReliablePoD(nasIP string, port int, sessionID string, secret []byte) error {
	pkt := radius.New(radius.CodeDisconnectRequest, secret)
	pkt.Set(radius.Type(44), []byte(sessionID)) // Acct-Session-Id

	return sendReliableControl(nasIP, port, secret, pkt, radius.CodeDisconnectACK, podAckTotal)
}
