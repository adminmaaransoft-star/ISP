package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/crm"
	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/pkg/validate"
)

// CRM lead pipeline endpoints — FR-CRM-001..003 | MDS §4.16.

// LeadQuerier is the persistence surface the lead pipeline needs.
// Satisfied by *db.CRMStore.
type LeadQuerier interface {
	CreateLead(ctx context.Context, l crm.Lead) (*crm.Lead, error)
	GetLead(ctx context.Context, id int) (*crm.Lead, error)
	ListLeads(ctx context.Context, status, source *string, franchiseID *int) ([]crm.Lead, error)
	UpdateLead(ctx context.Context, id int, status, assignedTo, notes, lostReason *string) (*crm.Lead, error)
	// ConvertLead must claim the lead and create the subscriber in one
	// transaction, returning (nil, nil, nil) when the lead was already
	// converted — that "did not land" signal is what stops two concurrent
	// conversions producing two subscribers.
	ConvertLead(ctx context.Context, leadID int, sub SubscriberRecord, passwordHash string) (*crm.Lead, *SubscriberRecord, error)
	GetFunnel(ctx context.Context, franchiseID *int) (*crm.FunnelReport, error)
}

type createLeadRequest struct {
	FullName     string `json:"full_name"`
	MobileNumber string `json:"mobile_number"`
	Email        string `json:"email"`
	Source       string `json:"source"`
	FranchiseID  *int   `json:"franchise_id"`
	AssignedTo   string `json:"assigned_to"`
	Notes        string `json:"notes"`
}

// CreateLead handles POST /api/v1/leads.
func (h *Handler) CreateLead(w http.ResponseWriter, r *http.Request) {
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}

	var req createLeadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.FullName == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "full_name is required")
		return
	}
	// Validated here as well as by chk_leads_mobile_e164: a lead's number
	// becomes a subscriber's number on conversion, so catching a malformed
	// one now beats rejecting it at the worst possible moment.
	if !validate.E164(req.MobileNumber) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"mobile_number must be E.164 format (e.g. +919876543210)")
		return
	}
	if !crm.ValidSource(req.Source) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"source must be one of walk_in, referral, website, campaign, franchise, other")
		return
	}

	// A franchise-scoped caller's leads belong to their own franchise,
	// taken from the token rather than the body — the same rule the
	// franchise endpoints follow, so an LCO cannot file leads against a
	// competitor's pipeline.
	franchiseID := req.FranchiseID
	if scope, ok := callerFranchiseScope(r); !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise binding")
		return
	} else if scope != nil {
		franchiseID = scope
	}

	created, err := h.leads.CreateLead(r.Context(), crm.Lead{
		FullName: req.FullName, MobileNumber: req.MobileNumber, Email: req.Email,
		Source: req.Source, FranchiseID: franchiseID,
		AssignedTo: req.AssignedTo, Notes: req.Notes,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create lead failed")
		return
	}

	crm.LeadsTotal.WithLabelValues(created.Status).Inc()
	middleware.Audit(r.Context(), "lead.create", strconv.Itoa(created.ID), map[string]any{
		"source": created.Source,
	})
	writeJSON(w, http.StatusCreated, created)
}

// ListLeads handles GET /api/v1/leads?status=&source=.
func (h *Handler) ListLeads(w http.ResponseWriter, r *http.Request) {
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}
	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise binding")
		return
	}

	var status, source *string
	if v := r.URL.Query().Get("status"); v != "" {
		status = &v
	}
	if v := r.URL.Query().Get("source"); v != "" {
		source = &v
	}

	list, err := h.leads.ListLeads(r.Context(), status, source, scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list leads failed")
		return
	}
	if list == nil {
		list = []crm.Lead{}
	}
	writeJSON(w, http.StatusOK, list)
}

// GetLead handles GET /api/v1/leads/{id}.
func (h *Handler) GetLead(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}

	lead, err := h.leads.GetLead(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "lead lookup failed")
		return
	}
	if lead == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "lead not found")
		return
	}
	if !callerMaySeeLead(r, lead) {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "not entitled to this lead")
		return
	}
	writeJSON(w, http.StatusOK, lead)
}

// callerMaySeeLead enforces the same per-partner isolation the franchise
// endpoints do (CRD-FRN-001): a franchise-scoped caller reaches only their
// own leads, decided by their token rather than the id in the path.
func callerMaySeeLead(r *http.Request, lead *crm.Lead) bool {
	scope, ok := callerFranchiseScope(r)
	if !ok {
		return false
	}
	if scope == nil {
		return true // ISP-wide staff
	}
	return lead.FranchiseID != nil && *lead.FranchiseID == *scope
}

type updateLeadRequest struct {
	Status     *string `json:"status"`
	AssignedTo *string `json:"assigned_to"`
	Notes      *string `json:"notes"`
	LostReason *string `json:"lost_reason"`
}

// UpdateLead handles PATCH /api/v1/leads/{id} — pipeline movement short of
// conversion.
func (h *Handler) UpdateLead(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}

	var req updateLeadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	// "converted" is deliberately not reachable here: reaching it also
	// requires creating a subscriber and setting converted_subscriber_id,
	// which only the convert endpoint does. Allowing it would produce a
	// converted lead pointing at nobody — a row
	// chk_lead_converted_has_subscriber refuses anyway, but as an opaque
	// 500 rather than this readable refusal.
	if req.Status != nil && !crm.ValidStatus(*req.Status) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"status must be one of new, contacted, qualified, lost (use /convert to convert)")
		return
	}

	existing, err := h.leads.GetLead(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "lead lookup failed")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "lead not found")
		return
	}
	if !callerMaySeeLead(r, existing) {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "not entitled to this lead")
		return
	}

	updated, err := h.leads.UpdateLead(r.Context(), id, req.Status, req.AssignedTo, req.Notes, req.LostReason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "update lead failed")
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "lead not found")
		return
	}

	if req.Status != nil {
		crm.LeadsTotal.WithLabelValues(*req.Status).Inc()
	}
	middleware.Audit(r.Context(), "lead.update", strconv.Itoa(id), map[string]any{"status": req.Status})
	writeJSON(w, http.StatusOK, updated)
}

// ── FR-CRM-002: Conversion ──────────────────────────────────────────────────

type convertLeadRequest struct {
	// The fields a subscriber needs and a lead does not. Name, mobile and
	// email are carried over from the lead rather than accepted here, so
	// the subscriber provably came from that prospect.
	CAFNumber       string `json:"caf_number"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	PlanID          int    `json:"plan_id"`
	RegisteredState string `json:"registered_state"`
	Aadhaar         string `json:"aadhaar,omitempty"`
	PAN             string `json:"pan,omitempty"`
	// CPESerial optionally issues a device in the same onboarding moment
	// (FR-INV-002). A failure to issue never fails the conversion.
	CPESerial string `json:"cpe_serial,omitempty"`
}

// convertLeadResponse reports all three outcomes of the handoff: the
// subscriber that now exists, the lead that produced them, and what happened
// to the CPE — including the case where the subscriber was created but the
// device could not be issued.
type convertLeadResponse struct {
	Subscriber *SubscriberRecord `json:"subscriber"`
	Lead       *crm.Lead         `json:"lead"`
	CPEIssued  string            `json:"cpe_issued,omitempty"`
	CPEWarning string            `json:"cpe_warning,omitempty"`
}

// ConvertLead handles POST /api/v1/leads/{id}/convert.
//
// Claims the lead and creates the subscriber atomically, carrying the
// prospect's name, mobile and email across, then optionally issues a CPE.
//
// FR: FR-CRM-002, FR-INV-002 | MDS §4.16
func (h *Handler) ConvertLead(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}

	var req convertLeadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}

	lead, err := h.leads.GetLead(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "lead lookup failed")
		return
	}
	if lead == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "lead not found")
		return
	}
	if !callerMaySeeLead(r, lead) {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "not entitled to this lead")
		return
	}
	if lead.Status == crm.StatusConverted {
		crm.ConversionConflictsTotal.Inc()
		writeError(w, http.StatusConflict, "ERR_ALREADY_CONVERTED", crm.ErrAlreadyConverted.Error())
		return
	}
	if lead.Status == crm.StatusLost {
		writeError(w, http.StatusConflict, "ERR_LEAD_LOST", crm.ErrLeadLost.Error())
		return
	}

	// The carry-over (FR-CRM-002): identity comes from the lead, account
	// details from the request.
	subReq := CreateSubscriberRequest{
		CAFNumber:       req.CAFNumber,
		Username:        req.Username,
		Password:        req.Password,
		MobileNumber:    lead.MobileNumber,
		Email:           lead.Email,
		PlanID:          req.PlanID,
		RegisteredState: req.RegisteredState,
	}
	if err := validateCreateSubscriber(subReq); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	hash, err := hashSubscriberPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "password hash failed")
		return
	}

	rec := subscriberRecordFrom(subReq)
	rec.FranchiseID = lead.FranchiseID // the partner who sourced the lead keeps the subscriber

	convertedLead, created, err := h.leads.ConvertLead(r.Context(), id, rec, hash)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "ERR_CONFLICT", "CAF number or username already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "lead conversion failed")
		return
	}
	if convertedLead == nil {
		// Lost the claim: another caller converted this lead between the
		// read above and the transaction. Exactly one subscriber exists,
		// and it is not ours.
		crm.ConversionConflictsTotal.Inc()
		writeError(w, http.StatusConflict, "ERR_ALREADY_CONVERTED", crm.ErrAlreadyConverted.Error())
		return
	}

	h.persistKYC(r.Context(), created.ID, req.Aadhaar, req.PAN)

	resp := convertLeadResponse{Subscriber: created, Lead: convertedLead}
	if req.CPESerial != "" {
		// Deliberately after the conversion committed, and never fatal to
		// it: the subscriber exists and can be billed whether or not the
		// warehouse could supply a router (MDS §4.16).
		resp.CPEIssued, resp.CPEWarning = h.issueCPEForConversion(r, req.CPESerial, created.ID)
	}

	crm.LeadsTotal.WithLabelValues(crm.StatusConverted).Inc()
	middleware.Audit(r.Context(), "lead.convert", strconv.Itoa(id), map[string]any{
		"subscriber_id": created.ID, "cpe_serial": resp.CPEIssued,
	})
	writeJSON(w, http.StatusCreated, resp)
}

// issueCPEForConversion issues a device as part of onboarding, returning
// (serial, "") on success and ("", warning) on any failure — never an error,
// because no CPE problem may undo a completed conversion.
func (h *Handler) issueCPEForConversion(r *http.Request, serial string, subscriberID int) (issued, warning string) {
	if h.inventory == nil {
		return "", "inventory is not configured; issue the device separately"
	}
	device, err := h.inventory.IssueDevice(r.Context(), serial, subscriberID)
	if err != nil {
		log.Error().Err(err).Str("serial", serial).Int("subscriber_id", subscriberID).
			Msg("api: CPE issuance failed during lead conversion")
		return "", "device could not be issued; the subscriber was created and the device can be issued separately"
	}
	if device == nil {
		return "", "device " + serial + " is not in stock; the subscriber was created and a device can be issued separately"
	}
	inventory.IssuedTotal.Inc()
	return device.SerialNumber, ""
}

// GetLeadFunnel handles GET /api/v1/leads/funnel (FR-CRM-003).
func (h *Handler) GetLeadFunnel(w http.ResponseWriter, r *http.Request) {
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}
	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise binding")
		return
	}

	report, err := h.leads.GetFunnel(r.Context(), scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "funnel report failed")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
