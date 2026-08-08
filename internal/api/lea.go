package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
)

// LEALookupRequest is the POST /api/v1/lea/lookup body.
type LEALookupRequest struct {
	PublicIP  string    `json:"public_ip"`
	Port      *int      `json:"port,omitempty"` // required if CGNAT in use
	Timestamp time.Time `json:"timestamp"`
}

// LEAResult identifies the subscriber holding an IP (and port, for CGNAT) at a
// point in time.
type LEAResult struct {
	SubscriberID    int        `json:"subscriber_id"`
	CAFNumber       string     `json:"caf_number"`
	Username        string     `json:"username"`
	MobileNumber    string     `json:"mobile_number"`
	RegisteredState string     `json:"registered_state"`
	SessionID       string     `json:"session_id,omitempty"`
	SessionStart    time.Time  `json:"session_start"`
	SessionStop     *time.Time `json:"session_stop,omitempty"`
	Source          string     `json:"source"` // "direct_ip" | "cgnat"
}

// LEAQuerier resolves a public IP (+ optional port) to the subscriber who held
// it at a given instant. Satisfied by *db.FUPStore.
type LEAQuerier interface {
	LookupByPublicIP(ctx context.Context, publicIP string, port *int, at time.Time) (*LEAResult, error)
}

// LEAAuditEntry is one row written to lea_audit_log.
type LEAAuditEntry struct {
	AccessorIdentity   string
	AccessorRole       string
	QueriedPublicIP    string
	QueriedPort        *int
	QueriedTimestamp   time.Time
	ResultSubscriberID *int
	ResultRowCount     int
}

// LEAAuditRecorder writes the append-only audit trail FR-OBS-003 requires.
// Satisfied by *db.FUPStore.
type LEAAuditRecorder interface {
	RecordLEAAudit(ctx context.Context, entry LEAAuditEntry) error
}

// LEALookup handles POST /api/v1/lea/lookup.
//
// Every lookup that reaches the database — a hit or a miss — writes an audit
// row before the response is sent: SecD §9.3 requires the tamper-evident trail
// to exist regardless of outcome, and writing it first means a client that
// never sees the response (a dropped connection) still leaves the record.
// A malformed request that fails validation before any query runs is not
// audited: no LEA data was accessed, so there is nothing to account for.
func (h *Handler) LEALookup(w http.ResponseWriter, r *http.Request) {
	if h.lea == nil || h.leaAudit == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "LEA lookup not configured")
		return
	}

	var req LEALookupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.PublicIP == "" || net.ParseIP(req.PublicIP) == nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "public_ip must be a valid IP address")
		return
	}
	if req.Timestamp.IsZero() {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "timestamp is required")
		return
	}

	result, lookupErr := h.lea.LookupByPublicIP(r.Context(), req.PublicIP, req.Port, req.Timestamp)

	rowCount := 0
	var resultSubID *int
	if lookupErr == nil && result != nil {
		rowCount = 1
		id := result.SubscriberID
		resultSubID = &id
	}

	if auditErr := h.leaAudit.RecordLEAAudit(r.Context(), LEAAuditEntry{
		AccessorIdentity:   middleware.SubjectFromContext(r.Context()),
		AccessorRole:       middleware.RoleFromContext(r.Context()),
		QueriedPublicIP:    req.PublicIP,
		QueriedPort:        req.Port,
		QueriedTimestamp:   req.Timestamp,
		ResultSubscriberID: resultSubID,
		ResultRowCount:     rowCount,
	}); auditErr != nil {
		// The audit write failing must not block the LEA response — the
		// requester already has their answer — but it must not be silent
		// either, since a missing audit row is itself a compliance gap.
		log.Error().Err(auditErr).Msg("api: LEA audit log write failed")
	}

	if lookupErr != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "lookup failed")
		return
	}
	if result == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no subscriber found for the given IP/port/timestamp")
		return
	}
	writeJSON(w, http.StatusOK, result)
}
