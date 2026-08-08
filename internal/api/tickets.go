package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// ticketCategories and ticketStatuses mirror the CHECK constraints on the
// tickets table. Validating here turns a bad enum value into a clean 422
// instead of a raw constraint-violation error surfacing as a 500.
var (
	ticketCategories = map[string]bool{"connectivity": true, "billing": true, "plan_change": true, "other": true}
	ticketStatuses   = map[string]bool{"open": true, "in_progress": true, "resolved": true, "closed": true}
)

// TicketRecord is the API representation of a support ticket.
type TicketRecord struct {
	ID           int       `json:"id"`
	SubscriberID int       `json:"subscriber_id"`
	Category     string    `json:"category"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	AssignedTo   *int      `json:"assigned_to,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TicketAdminQuerier creates and updates tickets on behalf of any subscriber.
// Distinct from the portal's ticket queries, which are scoped to the calling
// subscriber. Satisfied by *db.TicketStore.
type TicketAdminQuerier interface {
	CreateTicketAdmin(ctx context.Context, subscriberID int, category, description string) (*TicketRecord, error)
	UpdateTicketAdmin(ctx context.Context, ticketID int, status *string, assignedTo *int) (*TicketRecord, error)
}

// CreateTicket handles POST /api/v1/tickets.
func (h *Handler) CreateTicket(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "ticket store not configured")
		return
	}

	var req struct {
		SubscriberID int    `json:"subscriber_id"`
		Category     string `json:"category"`
		Description  string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.SubscriberID == 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "subscriber_id is required")
		return
	}
	if !ticketCategories[req.Category] {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "category must be one of connectivity, billing, plan_change, other")
		return
	}
	if req.Description == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "description is required")
		return
	}

	ticket, err := h.tickets.CreateTicketAdmin(r.Context(), req.SubscriberID, req.Category, req.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create ticket failed")
		return
	}

	middleware.Audit(r.Context(), "ticket.create", strconv.Itoa(ticket.ID), map[string]any{
		"subscriber_id": req.SubscriberID, "category": req.Category,
	})
	writeJSON(w, http.StatusCreated, ticket)
}

// UpdateTicket handles PATCH /api/v1/tickets/{ticket_id}.
func (h *Handler) UpdateTicket(w http.ResponseWriter, r *http.Request) {
	ticketID, err := pathInt(r, "ticket_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid ticket_id")
		return
	}
	if h.tickets == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "ticket store not configured")
		return
	}

	var req struct {
		Status     *string `json:"status"`
		AssignedTo *int    `json:"assigned_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Status != nil && !ticketStatuses[*req.Status] {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "status must be one of open, in_progress, resolved, closed")
		return
	}

	ticket, err := h.tickets.UpdateTicketAdmin(r.Context(), ticketID, req.Status, req.AssignedTo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "update ticket failed")
		return
	}
	if ticket == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "ticket not found")
		return
	}

	middleware.Audit(r.Context(), "ticket.update", strconv.Itoa(ticketID), map[string]any{
		"status": req.Status, "assigned_to": req.AssignedTo,
	})
	writeJSON(w, http.StatusOK, ticket)
}
