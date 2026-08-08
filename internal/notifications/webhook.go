package notifications

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

var (
	webhookHMACFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "notification_webhook_hmac_failures_total",
		Help: "Delivery-callback HMAC validation failures by provider",
	}, []string{"provider"})
	deliveryStatusTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "notification_delivery_status_total",
		Help: "Delivery status callbacks processed by status",
	}, []string{"status"})
)

// maxWebhookBody caps the request body read so a malicious caller cannot force
// unbounded allocation before the signature has been checked.
const maxWebhookBody = 1 << 20 // 1 MiB

// MetaWebhookPayload is the subset of Meta's delivery callback we consume.
type MetaWebhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				MessagingProduct string `json:"messaging_product"`
				Statuses         []struct {
					ID          string `json:"id"`
					Status      string `json:"status"`
					Timestamp   string `json:"timestamp"`
					RecipientID string `json:"recipient_id"`
				} `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// WebhookHandler processes Meta WhatsApp delivery status callbacks.
//
// FR: FR-NOTIF-011 | DDS §5.8
type WebhookHandler struct {
	db          NotifQuerier
	appSecret   string
	verifyToken string
}

// NewWebhookHandler constructs a WebhookHandler.
// appSecret is the Meta app secret used to verify X-Hub-Signature-256.
func NewWebhookHandler(db NotifQuerier, appSecret, verifyToken string) *WebhookHandler {
	return &WebhookHandler{db: db, appSecret: appSecret, verifyToken: verifyToken}
}

// ValidateMetaSignature verifies the X-Hub-Signature-256 header against the raw
// request body. Meta sends "sha256=<hex digest>".
//
// FR: FR-NOTIF-011, FR-SEC-004
func ValidateMetaSignature(body []byte, signatureHeader, appSecret string) error {
	digest := strings.TrimPrefix(signatureHeader, "sha256=")
	if digest == "" || digest == signatureHeader {
		webhookHMACFailures.WithLabelValues("meta").Inc()
		return fmt.Errorf("notifications: missing or malformed X-Hub-Signature-256 header")
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(digest)) {
		webhookHMACFailures.WithLabelValues("meta").Inc()
		return fmt.Errorf("notifications: invalid Meta webhook signature")
	}
	return nil
}

// Verify handles GET /webhooks/whatsapp — Meta's subscription handshake.
func (h *WebhookHandler) Verify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") != "subscribe" || q.Get("hub.verify_token") != h.verifyToken {
		http.Error(w, "verification failed", http.StatusForbidden)
		return
	}
	// Echo only characters Meta actually uses in a challenge, so the reflected
	// value can never carry markup back to a browser.
	_, _ = w.Write([]byte(SanitiseChallenge(q.Get("hub.challenge")))) //nolint:errcheck
}

// HandleDeliveryStatus handles POST /webhooks/whatsapp.
// It validates the Meta HMAC, then advances notification_log rows to the
// reported delivery status.
func (h *WebhookHandler) HandleDeliveryStatus(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	if err := ValidateMetaSignature(body, r.Header.Get("X-Hub-Signature-256"), h.appSecret); err != nil {
		log.Warn().Err(err).Msg("notifications: rejected WhatsApp webhook")
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var payload MetaWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			for _, st := range change.Value.Statuses {
				if st.ID == "" || st.Status == "" {
					continue
				}
				if err := h.db.UpdateDeliveryStatus(r.Context(), st.ID, st.Status); err != nil {
					// Meta retries on non-2xx, which would replay every status in
					// this batch. Log and keep going instead.
					log.Error().Err(err).
						Str("provider_message_id", st.ID).
						Str("status", st.Status).
						Msg("notifications: delivery status update failed")
					continue
				}
				deliveryStatusTotal.WithLabelValues(st.Status).Inc()
			}
		}
	}

	w.WriteHeader(http.StatusOK)
}

// SanitiseChallenge restricts Meta's echoed challenge to alphanumerics, hyphen
// and underscore, preventing reflected XSS via the query parameter.
func SanitiseChallenge(s string) string {
	out := make([]byte, 0, len(s))
	for i := range s {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-' || c == '_' {
			out = append(out, c)
		}
	}
	return string(out)
}
