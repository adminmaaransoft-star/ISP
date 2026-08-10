package billing_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

func testInvoiceData() billing.InvoiceData {
	return billing.InvoiceData{
		InvoiceNumber:   "INV-000042",
		InvoiceDate:     time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		DueDate:         time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
		SubscriberName:  "Test Subscriber",
		MobileNumber:    "+919876543210",
		RegisteredState: "TN",
		PlanName:        "TN_Super_100M",
		PlanPeriod:      "January 2026",
		BaseAmount:      decimal.RequireFromString("799.00"),
		CGSTRate:        decimal.RequireFromString("9.00"),
		CGSTAmount:      decimal.RequireFromString("71.91"),
		SGSTRate:        decimal.RequireFromString("9.00"),
		SGSTAmount:      decimal.RequireFromString("71.91"),
		TotalAmount:     decimal.RequireFromString("942.82"),
		GBUsed:          decimal.RequireFromString("120.00"),
		GBIncluded:      decimal.RequireFromString("3300.00"),
		SpeedActive:     "100 Mbps / 100 Mbps",
	}
}

func TestNewInvoicePDFClient(t *testing.T) {
	c := billing.NewInvoicePDFClient("http://gotenberg_engine:3000")
	if c == nil {
		t.Fatal("NewInvoicePDFClient returned nil")
	}
}

// TestFR_BIL_007_GeneratePDF_IncludesPlainLanguageUsageSummary verifies GeneratePDF sends a multipart form to
// Gotenberg's convert endpoint containing the rendered invoice HTML (proving
// the unexported renderInvoiceHTML worked — it has no exported entry point
// of its own), and returns exactly the bytes Gotenberg responds with.
func TestFR_BIL_007_GeneratePDF_IncludesPlainLanguageUsageSummary(t *testing.T) {
	const fakePDFBytes = "%PDF-1.4 fake gotenberg output"
	var gotPath, gotContentType string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakePDFBytes))
	}))
	defer srv.Close()

	c := billing.NewInvoicePDFClient(srv.URL)
	pdf, err := c.GeneratePDF(context.Background(), testInvoiceData())
	if err != nil {
		t.Fatalf("GeneratePDF: %v", err)
	}
	if string(pdf) != fakePDFBytes {
		t.Errorf("returned bytes: want %q, got %q", fakePDFBytes, string(pdf))
	}
	if gotPath != "/forms/chromium/convert/html" {
		t.Errorf("path: want /forms/chromium/convert/html, got %q", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Errorf("Content-Type: want multipart/form-data, got %q", gotContentType)
	}

	// The rendered HTML (renderInvoiceHTML's only observable output) must
	// have reached Gotenberg carrying the real invoice fields, GST-split
	// correctly, and the plain-language usage summary FR-BIL-007 requires.
	body := string(gotBody)
	for _, want := range []string{"INV-000042", "Test Subscriber", "TN_Super_100M", "942.82", "CGST", "120 GB of 3300 GB"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered HTML sent to Gotenberg missing %q", want)
		}
	}
}

// TestGeneratePDF_Interstate verifies the IGST branch of the invoice
// template renders when CGST/SGST are zero (SecD/DBD's mutually-exclusive
// GST rule — chk_gst_logic at the DB layer, mirrored here at the template
// layer).
func TestGeneratePDF_Interstate(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("%PDF-1.4"))
	}))
	defer srv.Close()

	data := testInvoiceData()
	data.CGSTRate, data.CGSTAmount = decimal.Zero, decimal.Zero
	data.SGSTRate, data.SGSTAmount = decimal.Zero, decimal.Zero
	data.IGSTRate = decimal.RequireFromString("18.00")
	data.IGSTAmount = decimal.RequireFromString("143.82")

	c := billing.NewInvoicePDFClient(srv.URL)
	if _, err := c.GeneratePDF(context.Background(), data); err != nil {
		t.Fatalf("GeneratePDF: %v", err)
	}
	if !strings.Contains(string(gotBody), "IGST") {
		t.Error("expected the IGST line item to render when CGST/SGST are both zero")
	}
}

func TestGeneratePDF_GotenbergError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("chromium crashed"))
	}))
	defer srv.Close()

	c := billing.NewInvoicePDFClient(srv.URL)
	_, err := c.GeneratePDF(context.Background(), testInvoiceData())
	if err == nil {
		t.Fatal("expected an error when Gotenberg returns a non-200 status")
	}
}

func TestGeneratePDF_GotenbergUnreachable(t *testing.T) {
	// A closed server: connection refused, exercising the httpClient.Do error
	// path rather than the non-200-status path above.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close()

	c := billing.NewInvoicePDFClient(srv.URL)
	_, err := c.GeneratePDF(context.Background(), testInvoiceData())
	if err == nil {
		t.Fatal("expected an error when Gotenberg is unreachable")
	}
}
