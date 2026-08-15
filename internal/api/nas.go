package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// NAS device management — FR-NAS-001..004, FR-HSP-002 | MDS §4.11, §4.23.
//
// Registering a NAS was previously a direct INSERT. That made
// nas_devices.allow_mab — the one prerequisite a hotspot deployment cannot skip
// — reachable only from a psql prompt, and it meant the RADIUS shared secret
// was encrypted by hand or, more likely, not at all.
//
// Two things this surface must never do, both structural rather than
// conventional: return a secret in any form (the store's summary query does not
// select the column), and accept a plaintext secret it then stores as-is (every
// write path here encrypts through the configured key store first).

// NASQuerier is the persistence surface for NAS management.
// Satisfied by *db.NASStore.
type NASQuerier interface {
	ListNASDeviceSummaries(ctx context.Context) ([]nas.DeviceSummary, error)
	CreateNASDevice(ctx context.Context, d nas.NewNASDevice) (*nas.DeviceSummary, error)
	UpdateNASDevice(ctx context.Context, id int, u nas.NASDeviceUpdate) (*nas.DeviceSummary, error)
}

type createNASRequest struct {
	IP           string `json:"ip"`
	Vendor       string `json:"vendor"`
	Description  string `json:"description,omitempty"`
	RadiusSecret string `json:"radius_secret"`
	CoAPort      *int   `json:"coa_port,omitempty"`
	PoDPort      *int   `json:"pod_port,omitempty"`
	AllowMAB     bool   `json:"allow_mab,omitempty"`
}

// defaultControlPort is MikroTik's CoA/PoD listener. RFC 5176 specifies 3799,
// so this is a per-device setting with the value most deployments here need.
const defaultControlPort = 1700

// ListNASDevices handles GET /api/v1/nas.
func (h *Handler) ListNASDevices(w http.ResponseWriter, r *http.Request) {
	if h.nas == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "NAS management is not configured")
		return
	}
	devices, err := h.nas.ListNASDeviceSummaries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list NAS devices failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nas_devices": devices,
		// Surfaced so an operator picking a vendor does not have to guess at the
		// accepted values and get a 422 to find out.
		"supported_vendors": nas.Vendors(),
	})
}

// CreateNASDevice handles POST /api/v1/nas.
func (h *Handler) CreateNASDevice(w http.ResponseWriter, r *http.Request) {
	if h.nas == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "NAS management is not configured")
		return
	}
	if h.secretEncryptor == nil {
		// Refused rather than stored in the clear. A NAS row whose secret is
		// plaintext looks identical to a correct one until someone reads the
		// table.
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE",
			"no encryption key is configured; a RADIUS secret cannot be stored safely")
		return
	}

	var req createNASRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if err := validateNASRequest(req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	encrypted, err := h.secretEncryptor.Encrypt(req.RadiusSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not encrypt the RADIUS secret")
		return
	}

	device, err := h.nas.CreateNASDevice(r.Context(), nas.NewNASDevice{
		IP:              req.IP,
		Vendor:          req.Vendor,
		Description:     req.Description,
		SecretEncrypted: encrypted,
		KeyVersion:      h.secretEncryptor.ActiveVersion(),
		CoAPort:         portOrDefault(req.CoAPort),
		PoDPort:         portOrDefault(req.PoDPort),
		AllowMAB:        req.AllowMAB,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "ERR_CONFLICT", "a NAS with that IP is already registered")
			return
		}
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not register the NAS")
		return
	}

	// allow_mab is called out in the audit entry because it is the field that
	// changes what the network will accept, not merely how it is shaped.
	middleware.Audit(r.Context(), "nas.registered", req.IP, map[string]any{
		"vendor": req.Vendor, "allow_mab": req.AllowMAB,
	})
	writeJSON(w, http.StatusCreated, nasResponse(device))
}

type updateNASRequest struct {
	Vendor       *string `json:"vendor,omitempty"`
	Description  *string `json:"description,omitempty"`
	RadiusSecret *string `json:"radius_secret,omitempty"`
	CoAPort      *int    `json:"coa_port,omitempty"`
	PoDPort      *int    `json:"pod_port,omitempty"`
	AllowMAB     *bool   `json:"allow_mab,omitempty"`
}

// UpdateNASDevice handles PATCH /api/v1/nas/{id} — including the allow_mab
// toggle a hotspot deployment needs.
func (h *Handler) UpdateNASDevice(w http.ResponseWriter, r *http.Request) {
	if h.nas == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "NAS management is not configured")
		return
	}
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "id must be numeric")
		return
	}

	var req updateNASRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Vendor != nil && !nas.KnownVendor(nas.Vendor(*req.Vendor)) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"vendor must be one of: "+strings.Join(nas.Vendors(), ", "))
		return
	}
	if err := validatePorts(req.CoAPort, req.PoDPort); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	update := nas.NASDeviceUpdate{
		Vendor:      req.Vendor,
		Description: req.Description,
		CoAPort:     req.CoAPort,
		PoDPort:     req.PoDPort,
		AllowMAB:    req.AllowMAB,
	}
	if req.RadiusSecret != nil {
		if *req.RadiusSecret == "" {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
				"radius_secret cannot be set to empty; omit the field to leave it unchanged")
			return
		}
		if h.secretEncryptor == nil {
			writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE",
				"no encryption key is configured; a RADIUS secret cannot be stored safely")
			return
		}
		encrypted, err := h.secretEncryptor.Encrypt(*req.RadiusSecret)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not encrypt the RADIUS secret")
			return
		}
		version := h.secretEncryptor.ActiveVersion()
		update.SecretEncrypted = &encrypted
		update.KeyVersion = &version
	}

	device, err := h.nas.UpdateNASDevice(r.Context(), id, update)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "update failed")
		return
	}
	if device == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no NAS with that id")
		return
	}

	middleware.Audit(r.Context(), "nas.updated", device.IP, map[string]any{
		"allow_mab": req.AllowMAB, "vendor": req.Vendor, "secret_rotated": req.RadiusSecret != nil,
	})
	writeJSON(w, http.StatusOK, nasResponse(device))
}

// nasResponse wraps a device with the one operational fact that is not visible
// from the row: changes are not instant.
//
// nas.Resolver caches the inventory and refreshes on a 60-second interval, so
// an operator who enables allow_mab and immediately tests a MAC will see it
// refused and reasonably conclude the toggle did not work.
func nasResponse(device *nas.DeviceSummary) map[string]any {
	return map[string]any{
		"nas_device": device,
		"note":       "RADIUS daemons refresh their NAS cache every 60s; changes take effect within that window.",
	}
}

func validateNASRequest(req createNASRequest) error {
	if net.ParseIP(req.IP) == nil {
		return fmt.Errorf("ip must be a valid IP address")
	}
	if !nas.KnownVendor(nas.Vendor(req.Vendor)) {
		return fmt.Errorf("vendor must be one of: %s", strings.Join(nas.Vendors(), ", "))
	}
	if req.RadiusSecret == "" {
		return fmt.Errorf("radius_secret is required")
	}
	// Short shared secrets are the classic RADIUS weakness: the protocol uses
	// them to key an MD5 stream, so a guessable one exposes every password on
	// the wire. RFC 5080 §2.3 recommends at least 16 characters.
	if len(req.RadiusSecret) < 16 {
		return fmt.Errorf("radius_secret must be at least 16 characters")
	}
	return validatePorts(req.CoAPort, req.PoDPort)
}

func validatePorts(coaPort, podPort *int) error {
	for name, p := range map[string]*int{"coa_port": coaPort, "pod_port": podPort} {
		if p != nil && (*p < 1 || *p > 65535) {
			return fmt.Errorf("%s must be between 1 and 65535", name)
		}
	}
	return nil
}

func portOrDefault(p *int) int {
	if p == nil || *p == 0 {
		return defaultControlPort
	}
	return *p
}
