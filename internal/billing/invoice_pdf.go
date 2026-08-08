// Package billing â€” invoice PDF generation via Gotenberg.
//
// FR: FR-BIL-007 | DDS Â§5.6 | MDS Â§4.3
// Gotenberg HTMLâ†’PDF endpoint: POST http://gotenberg_engine:3000/forms/chromium/convert/html
package billing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"text/template"
	"time"

	"github.com/shopspring/decimal"
)

// InvoiceData carries all fields rendered into the PDF template.
type InvoiceData struct {
	InvoiceNumber   string
	InvoiceDate     time.Time
	DueDate         time.Time
	SubscriberName  string
	MobileNumber    string
	RegisteredState string
	PlanName        string
	PlanPeriod      string // e.g. "June 2025"

	// Billing amounts
	BaseAmount  decimal.Decimal
	CGSTRate    decimal.Decimal
	CGSTAmount  decimal.Decimal
	SGSTRate    decimal.Decimal
	SGSTAmount  decimal.Decimal
	IGSTRate    decimal.Decimal
	IGSTAmount  decimal.Decimal
	TotalAmount decimal.Decimal

	// Usage summary block (FR-BIL-007 plain-language requirement)
	GBUsed      decimal.Decimal
	GBIncluded  decimal.Decimal
	SpeedActive string // e.g. "100 Mbps / 100 Mbps"
	FUPApplied  bool
}

// InvoicePDFClient generates GST-compliant PDF invoices via Gotenberg.
type InvoicePDFClient struct {
	gotenbergURL string
	httpClient   *http.Client
}

// NewInvoicePDFClient constructs an InvoicePDFClient.
// gotenbergURL example: "http://gotenberg_engine:3000"
func NewInvoicePDFClient(gotenbergURL string) *InvoicePDFClient {
	return &InvoicePDFClient{
		gotenbergURL: gotenbergURL,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
}

// GeneratePDF renders the invoice HTML template and sends it to Gotenberg,
// returning the raw PDF bytes.
//
// FR: FR-BIL-007 | DDS Â§5.6
func (c *InvoicePDFClient) GeneratePDF(ctx context.Context, data InvoiceData) ([]byte, error) {
	html, err := renderInvoiceHTML(data)
	if err != nil {
		return nil, fmt.Errorf("billing: render invoice HTML: %w", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	fw, err := mw.CreateFormFile("files", "index.html")
	if err != nil {
		return nil, fmt.Errorf("billing: create form file: %w", err)
	}
	if _, err := fw.Write([]byte(html)); err != nil {
		return nil, fmt.Errorf("billing: write html to form: %w", err)
	}
	_ = mw.Close() // multipart writer close flushes the boundary; error not recoverable here

	req, err := http.NewRequestWithContext(ctx,
		http.MethodPost,
		c.gotenbergURL+"/forms/chromium/convert/html",
		&buf,
	)
	if err != nil {
		return nil, fmt.Errorf("billing: create gotenberg request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("billing: gotenberg request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("billing: gotenberg returned %d: %s", resp.StatusCode, body)
	}

	return io.ReadAll(resp.Body)
}

// invoiceHTMLTemplate is the GST-compliant plain-language invoice layout.
var invoiceHTMLTemplate = template.Must(template.New("invoice").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<style>
  body { font-family: Arial, sans-serif; font-size: 12px; margin: 40px; color: #222; }
  h1   { font-size: 20px; }
  table { border-collapse: collapse; width: 100%; }
  th, td { border: 1px solid #ccc; padding: 6px 10px; text-align: left; }
  th { background: #f5f5f5; }
  .total td { font-weight: bold; background: #eef; }
  .usage-box { background: #f0f8ff; border: 1px solid #90caf9; border-radius: 6px;
               padding: 12px 16px; margin: 24px 0; }
  .usage-box h3 { margin: 0 0 8px; font-size: 13px; color: #1565c0; }
  .usage-row { display: flex; justify-content: space-between; margin: 4px 0; }
  .footer { margin-top: 24px; font-size: 10px; color: #666; }
</style>
</head>
<body>
  <h1>GST Tax Invoice</h1>
  <p><strong>Invoice Number:</strong> {{.InvoiceNumber}} &nbsp;&nbsp;
     <strong>Date:</strong> {{.InvoiceDate.Format "02 Jan 2006"}} &nbsp;&nbsp;
     <strong>Due Date:</strong> {{.DueDate.Format "02 Jan 2006"}}</p>

  <table style="margin-bottom:16px">
    <tr><th>Subscriber Name</th><td>{{.SubscriberName}}</td>
        <th>Mobile</th><td>{{.MobileNumber}}</td></tr>
    <tr><th>Plan</th><td>{{.PlanName}}</td>
        <th>Period</th><td>{{.PlanPeriod}}</td></tr>
    <tr><th>State (for GST)</th><td colspan="3">{{.RegisteredState}}</td></tr>
  </table>

  <table>
    <tr><th>Description</th><th>Amount (â‚¹)</th></tr>
    <tr><td>Internet Plan â€” {{.PlanName}}</td><td>{{.BaseAmount.StringFixed 2}}</td></tr>
    {{if gt .CGSTRate.Sign 0}}
    <tr><td>CGST @ {{.CGSTRate}}%</td><td>{{.CGSTAmount.StringFixed 2}}</td></tr>
    <tr><td>SGST @ {{.SGSTRate}}%</td><td>{{.SGSTAmount.StringFixed 2}}</td></tr>
    {{else}}
    <tr><td>IGST @ {{.IGSTRate}}%</td><td>{{.IGSTAmount.StringFixed 2}}</td></tr>
    {{end}}
    <tr class="total"><td>Total</td><td>â‚¹{{.TotalAmount.StringFixed 2}}</td></tr>
  </table>

  <!-- Usage Summary Block â€” FR-BIL-007 plain-language requirement -->
  <div class="usage-box">
    <h3>Data Usage Summary</h3>
    <div class="usage-row">
      <span>Data used this cycle:</span>
      <strong>{{.GBUsed.StringFixed 0}} GB of {{.GBIncluded.StringFixed 0}} GB included</strong>
    </div>
    <div class="usage-row">
      <span>Speed applied:</span>
      {{if .FUPApplied}}
      <strong style="color:#c62828">FUP throttle active â€” {{.SpeedActive}}</strong>
      {{else}}
      <strong style="color:#2e7d32">{{.SpeedActive}} (full speed)</strong>
      {{end}}
    </div>
  </div>

  <p class="footer">
    This is a computer-generated invoice. For queries, contact support.<br>
    HSN/SAC: 998432 (Internet Telecommunication Services) | GST applies under the reverse-charge mechanism for B2B.
  </p>
</body>
</html>
`))

func renderInvoiceHTML(data InvoiceData) (string, error) {
	var buf bytes.Buffer
	if err := invoiceHTMLTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
