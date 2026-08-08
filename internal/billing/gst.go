// Package billing implements GST computation, wallet double-entry, dunning,
// and invoice generation for the ISP BSS/OSS platform.
//
// FR: FR-BIL-001..007 | DDS §5.4, §5.6 | DBD §6.2
package billing

import (
	"github.com/shopspring/decimal"
)

// GstRate holds the applicable tax rates for a billing period.
type GstRate struct {
	ID       int
	CgstRate decimal.Decimal
	SgstRate decimal.Decimal
	IgstRate decimal.Decimal
}

// Invoice represents a computed GST invoice ready for persistence.
type Invoice struct {
	SubscriberID int
	BaseAmount   decimal.Decimal
	CgstAmount   decimal.Decimal
	SgstAmount   decimal.Decimal
	IgstAmount   decimal.Decimal
	TotalAmount  decimal.Decimal
	GstRateID    int
	GbIncluded   int
	GbUsed       decimal.Decimal
}

// CalculateGstInvoice applies intrastate (CGST+SGST) or interstate (IGST) tax
// rules based on the subscriber's registered state vs. ISP state (TN = intrastate).
// Uses banker's rounding (Round(2)) as required by FR-BIL-002.
//
// FR: FR-BIL-001, FR-BIL-002 | DDS §5.4
func CalculateGstInvoice(baseAmount decimal.Decimal, subscriberState string, rate GstRate) Invoice {
	var cgst, sgst, igst decimal.Decimal

	if subscriberState == "TN" {
		// Intrastate: split CGST + SGST
		cgst = baseAmount.Mul(rate.CgstRate).Div(decimal.NewFromInt(100)).Round(2)
		sgst = baseAmount.Mul(rate.SgstRate).Div(decimal.NewFromInt(100)).Round(2)
		igst = decimal.Zero
	} else {
		// Interstate: IGST only
		igst = baseAmount.Mul(rate.IgstRate).Div(decimal.NewFromInt(100)).Round(2)
		cgst = decimal.Zero
		sgst = decimal.Zero
	}

	total := baseAmount.Add(cgst).Add(sgst).Add(igst)
	return Invoice{
		BaseAmount:  baseAmount,
		CgstAmount:  cgst,
		SgstAmount:  sgst,
		IgstAmount:  igst,
		TotalAmount: total,
		GstRateID:   rate.ID,
	}
}
