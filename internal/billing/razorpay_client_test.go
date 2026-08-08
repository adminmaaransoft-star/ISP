package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"
)

func newTestRazorpayClient(t *testing.T, handler http.HandlerFunc) *RazorpayClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewRazorpayClient("key_id", "key_secret")
	c.baseURL = srv.URL
	return c
}

func TestRazorpayClient_CreateOrder_Success(t *testing.T) {
	var gotBody razorpayPaymentLinkRequest
	c := newTestRazorpayClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "key_id" || pass != "key_secret" {
			t.Errorf("expected basic auth key_id:key_secret, got %q:%q (ok=%v)", user, pass, ok)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(razorpayPaymentLinkResponse{
			ID:       "plink_abc123",
			ShortURL: "https://rzp.io/i/abc123",
		})
	})

	orderID, link, err := c.CreateOrder(context.Background(), 42, decimal.RequireFromString("799.00"))
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if orderID != "plink_abc123" {
		t.Errorf("orderID = %q, want plink_abc123", orderID)
	}
	if link != "https://rzp.io/i/abc123" {
		t.Errorf("link = %q, want https://rzp.io/i/abc123", link)
	}
	if gotBody.Amount != 79900 {
		t.Errorf("amount = %d paise, want 79900", gotBody.Amount)
	}
	if gotBody.Currency != "INR" {
		t.Errorf("currency = %q, want INR", gotBody.Currency)
	}
	if gotBody.Notes["subscriber_id"] != "42" {
		t.Errorf("notes[subscriber_id] = %q, want 42", gotBody.Notes["subscriber_id"])
	}
}

func TestRazorpayClient_CreateOrder_GatewayError(t *testing.T) {
	c := newTestRazorpayClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"description": "amount must be at least 100"},
		})
	})

	_, _, err := c.CreateOrder(context.Background(), 42, decimal.RequireFromString("799.00"))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestRazorpayClient_CreateOrder_RejectsNonPositiveAmount(t *testing.T) {
	c := NewRazorpayClient("key_id", "key_secret")
	for _, amt := range []string{"0", "-5.00"} {
		_, _, err := c.CreateOrder(context.Background(), 1, decimal.RequireFromString(amt))
		if err == nil {
			t.Errorf("amount %s: expected an error, got nil", amt)
		}
	}
}

func TestRazorpayClient_CreateOrder_MissingIDOrLink(t *testing.T) {
	c := newTestRazorpayClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(razorpayPaymentLinkResponse{})
	})

	_, _, err := c.CreateOrder(context.Background(), 1, decimal.RequireFromString("100.00"))
	if err == nil {
		t.Fatal("expected an error for missing id/short_url, got nil")
	}
}
