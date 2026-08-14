package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/tr069"
)

// NOC-facing TR-069 control — FR-CPE-003 | MDS §4.19.
//
// Every endpoint here *queues* an RPC rather than performing one. CWMP is
// CPE-initiated and, behind CGNAT, a Connection Request frequently cannot
// reach the device — so the honest contract is "this will run when the
// device next checks in", and the API says so by answering 202 with the
// queued task rather than 200 with a result.

// CPEControlQuerier is the persistence surface NOC control needs.
// Satisfied by *db.TR069Store.
type CPEControlQuerier interface {
	GetDeviceBySerialACS(ctx context.Context, serial string) (*tr069.Device, error)
	EnqueueTask(ctx context.Context, deviceID int, rpcType string, params map[string]string, priority int, createdBy string) (*tr069.Task, error)
	ListTasks(ctx context.Context, deviceID int, status *string) ([]tr069.Task, error)
	SetProvisioningState(ctx context.Context, deviceID int, state, lastFault string) error
}

// resolveDevice looks a device up by serial, writing the error response
// itself and returning nil when it cannot be found.
func (h *Handler) resolveDevice(w http.ResponseWriter, r *http.Request) *tr069.Device {
	serial := r.PathValue("serial")
	if serial == "" {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "serial is required")
		return nil
	}
	if h.cpeControl == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "TR-069 ACS not configured")
		return nil
	}

	device, err := h.cpeControl.GetDeviceBySerialACS(r.Context(), serial)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "device lookup failed")
		return nil
	}
	if device == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no device with that serial number")
		return nil
	}
	return device
}

// queueRPC is the shared tail of every control endpoint.
func (h *Handler) queueRPC(w http.ResponseWriter, r *http.Request, device *tr069.Device,
	rpcType string, params map[string]string, priority int, auditAction string,
) {
	task, err := h.cpeControl.EnqueueTask(r.Context(), device.ID, rpcType, params, priority,
		middleware.SubjectFromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not queue the RPC")
		return
	}

	middleware.Audit(r.Context(), auditAction, device.SerialNumber, map[string]any{
		"task_id": task.ID, "rpc_type": rpcType,
	})
	// 202, not 200: nothing has happened to the device yet, and saying
	// otherwise would have a technician standing next to a router that has
	// not rebooted wondering why.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task":           task,
		"delivery":       "queued",
		"delivered_when": "the device opens its next CWMP session (periodic Inform, or sooner on reboot)",
		"last_inform_at": device.LastInformAt,
	})
}

// RebootCPE handles POST /api/v1/cpe/devices/{serial}/reboot.
func (h *Handler) RebootCPE(w http.ResponseWriter, r *http.Request) {
	device := h.resolveDevice(w, r)
	if device == nil {
		return
	}
	// Priority 10: a reboot a technician is waiting on should overtake
	// routine provisioning already sitting in the queue.
	h.queueRPC(w, r, device, tr069.RPCReboot, nil, 10, "cpe.reboot")
}

type firmwareRequest struct {
	URL string `json:"url"`
}

// UpgradeCPEFirmware handles POST /api/v1/cpe/devices/{serial}/firmware.
func (h *Handler) UpgradeCPEFirmware(w http.ResponseWriter, r *http.Request) {
	device := h.resolveDevice(w, r)
	if device == nil {
		return
	}

	var req firmwareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	// A Download with no URL, or one the device cannot fetch, can leave a
	// router half-upgraded — so the obvious mistakes are refused here rather
	// than on the hardware.
	if req.URL == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "url is required")
		return
	}
	if !hasHTTPScheme(req.URL) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"url must be an http:// or https:// address the device can reach")
		return
	}

	h.queueRPC(w, r, device, tr069.RPCDownload, map[string]string{"url": req.URL}, 20, "cpe.firmware")
}

type getParamsRequest struct {
	Names []string `json:"names"`
}

// GetCPEParameters handles POST /api/v1/cpe/devices/{serial}/parameters —
// queueing a diagnostic read.
func (h *Handler) GetCPEParameters(w http.ResponseWriter, r *http.Request) {
	device := h.resolveDevice(w, r)
	if device == nil {
		return
	}

	var req getParamsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if len(req.Names) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "at least one parameter name is required")
		return
	}

	params := make(map[string]string, len(req.Names))
	for i, n := range req.Names {
		params[strconv.Itoa(i)] = n
	}
	h.queueRPC(w, r, device, tr069.RPCGetParameterValues, params, 50, "cpe.get_parameters")
}

// ReprovisionCPE handles POST /api/v1/cpe/devices/{serial}/reprovision.
//
// Marks the device as needing configuration rather than queueing the RPC
// directly: the ACS rebuilds the parameter set from the subscriber's current
// plan when the device next Informs, so a plan change between now and then
// is reflected rather than baked in at button-press time.
func (h *Handler) ReprovisionCPE(w http.ResponseWriter, r *http.Request) {
	device := h.resolveDevice(w, r)
	if device == nil {
		return
	}

	if err := h.cpeControl.SetProvisioningState(r.Context(), device.ID, tr069.StateNeedsReprovision, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not flag the device for reprovisioning")
		return
	}

	middleware.Audit(r.Context(), "cpe.reprovision", device.SerialNumber, nil)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"serial_number":  device.SerialNumber,
		"state":          tr069.StateNeedsReprovision,
		"delivered_when": "the device opens its next CWMP session",
		"last_inform_at": device.LastInformAt,
	})
}

// ListCPETasks handles GET /api/v1/cpe/devices/{serial}/tasks?status=.
func (h *Handler) ListCPETasks(w http.ResponseWriter, r *http.Request) {
	device := h.resolveDevice(w, r)
	if device == nil {
		return
	}

	var status *string
	if v := r.URL.Query().Get("status"); v != "" {
		status = &v
	}

	tasks, err := h.cpeControl.ListTasks(r.Context(), device.ID, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list CPE tasks failed")
		return
	}
	if tasks == nil {
		tasks = []tr069.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": device, "tasks": tasks})
}

// hasHTTPScheme reports whether a URL starts with an http(s) scheme.
func hasHTTPScheme(u string) bool {
	return len(u) > 7 && (u[:7] == "http://" || (len(u) > 8 && u[:8] == "https://"))
}
