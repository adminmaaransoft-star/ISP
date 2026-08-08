package portalui

import (
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
)

type notificationsData struct {
	baseData
	Notifications []portal.NotificationEntry
}

// Notifications handles GET /ui/notifications.
func (h *Handler) Notifications(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())

	var notifications []portal.NotificationEntry
	if h.notifications != nil {
		var err error
		notifications, err = h.notifications.ListNotifications(r.Context(), subID, 50)
		if err != nil {
			http.Error(w, "failed to load notifications", http.StatusInternalServerError)
			return
		}
	}

	renderPage(w, http.StatusOK, "notifications", notificationsData{
		baseData:      baseData{Authenticated: true, Active: "notifications"},
		Notifications: notifications,
	})
}
