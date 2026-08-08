package portalui

import (
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
)

type dashboardData struct {
	baseData
	Profile *portal.SubscriberProfile
	Session *portal.ActiveSession
}

// Dashboard handles GET /ui/dashboard — the browsable equivalent of the JSON
// GET /portal/dashboard, reading from the same queriers portal.Handler uses.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())
	profile, err := h.subscribers.GetSubscriberByID(r.Context(), subID)
	if err != nil || profile == nil {
		http.Error(w, "subscriber not found", http.StatusNotFound)
		return
	}

	var session *portal.ActiveSession
	if h.sessions != nil {
		session, _ = h.sessions.GetActiveSession(r.Context(), subID)
	}

	renderPage(w, http.StatusOK, "dashboard", dashboardData{
		baseData: baseData{Authenticated: true, Active: "dashboard"},
		Profile:  profile,
		Session:  session,
	})
}

// DashboardSessionFragment handles GET /ui/dashboard/session — the HTMX
// polling target (every 15s) for the live-usage card, so it refreshes
// without a full page reload.
func (h *Handler) DashboardSessionFragment(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())
	var session *portal.ActiveSession
	if h.sessions != nil {
		session, _ = h.sessions.GetActiveSession(r.Context(), subID)
	}
	renderFragment(w, "session-inner", session)
}
