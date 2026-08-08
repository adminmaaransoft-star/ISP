package notifications

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// MSG91Client implements SMSSender for the MSG91 SMS gateway.
//
// FR: FR-NOTIF-009 (SMS channel) | DDS Â§5.8
type MSG91Client struct {
	apiKey   string
	senderID string
	http     *http.Client
}

// NewMSG91Client constructs an MSG91Client.
func NewMSG91Client(apiKey, senderID string) *MSG91Client {
	return &MSG91Client{apiKey: apiKey, senderID: senderID, http: &http.Client{}}
}

// SendSMS sends a plain-text SMS via MSG91.
func (c *MSG91Client) SendSMS(ctx context.Context, toPhone, message string) error {
	// Sanitise: strip leading '+' for MSG91 (expects 91XXXXXXXXXX format)
	mobile := strings.TrimPrefix(toPhone, "+")

	params := url.Values{}
	params.Set("authkey", c.apiKey)
	params.Set("mobiles", mobile)
	params.Set("message", message)
	params.Set("sender", c.senderID)
	params.Set("route", "4") // transactional route

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.msg91.com/api/sendhttp.php?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("sms: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sms: msg91 send: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 400 {
		return fmt.Errorf("sms: msg91 returned HTTP %d", resp.StatusCode)
	}
	return nil
}
