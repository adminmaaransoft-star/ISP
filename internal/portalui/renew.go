package portalui

import (
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

type renewData struct {
	baseData
	CSRFToken string
}

// RenewPage handles GET /ui/renew.
func (h *Handler) RenewPage(w http.ResponseWriter, r *http.Request) {
	renderPage(w, http.StatusOK, "renew", renewData{
		baseData:  baseData{Authenticated: true, Active: "renew"},
		CSRFToken: h.csrfTokenFromRequest(r),
	})
}

type renewResultData struct {
	Error       string
	Amount      string
	PaymentLink string
}

// Renew handles POST /ui/renew. Guarded by requireCSRF (see auth.go and
// RegisterRoutes) since, unlike the JSON API's Bearer-only POST
// /portal/renew, this route is reachable via a cookie the browser attaches
// automatically. Returns only the result fragment (an HTMX target), not a
// full page — the form stays on screen either way.
func (h *Handler) Renew(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())

	amount, err := decimal.NewFromString(r.FormValue("amount"))
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		renderFragment(w, "renew-result", renewResultData{Error: "Enter a valid amount."})
		return
	}

	if h.razorpay == nil {
		renderFragment(w, "renew-result", renewResultData{Error: "Payment gateway is not configured."})
		return
	}

	_, paymentLink, err := h.razorpay.CreateOrder(r.Context(), subID, amount)
	if err != nil {
		renderFragment(w, "renew-result", renewResultData{Error: "Payment gateway error. Please try again."})
		return
	}

	renderFragment(w, "renew-result", renewResultData{
		Amount:      amount.StringFixed(2),
		PaymentLink: paymentLink,
	})
}
