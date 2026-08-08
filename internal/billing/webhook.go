package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var webhookHMACFailures = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "billing_webhook_hmac_failures_total",
	Help: "Webhook HMAC validation failures by provider",
}, []string{"provider"})

// ValidateRazorpaySignature verifies the X-Razorpay-Signature header.
// Razorpay signs the raw body with HMAC-SHA256 using the webhook secret.
//
// FR: FR-SEC-004, FR-BIL-005 | DDS §5.6
func ValidateRazorpaySignature(payload []byte, signature, secret string) error {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		webhookHMACFailures.WithLabelValues("razorpay").Inc()
		return fmt.Errorf("billing: invalid Razorpay webhook signature")
	}
	return nil
}
