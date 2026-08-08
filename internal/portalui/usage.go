package portalui

import (
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
)

type usageData struct {
	baseData
	Sessions []portal.SessionHistoryEntry
}

// Usage handles GET /ui/usage — the subscriber's past (and, if one exists,
// currently active) internet sessions.
func (h *Handler) Usage(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())

	var sessions []portal.SessionHistoryEntry
	if h.sessionHistory != nil {
		var err error
		sessions, err = h.sessionHistory.ListSessionHistory(r.Context(), subID, 50)
		if err != nil {
			http.Error(w, "failed to load usage history", http.StatusInternalServerError)
			return
		}
	}

	renderPage(w, http.StatusOK, "usage", usageData{
		baseData: baseData{Authenticated: true, Active: "usage"},
		Sessions: sessions,
	})
}
