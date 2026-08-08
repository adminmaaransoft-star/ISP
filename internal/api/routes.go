// Package api wires all HTTP routes for the BSS/OSS API service.
//
// FR: FR-AAA-001..004, FR-BIL-001..007, FR-NET-001..003, FR-SUB-001..005,
//
//	FR-OBS-004, FR-SEC-005 | DDS §5.7, §5.9 | API §7
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/bcrypt"
)

// SubscriberRecord is the API representation of a subscriber.
type SubscriberRecord struct {
	ID              int        `json:"id"`
	CAFNumber       string     `json:"caf_number"`
	Username        string     `json:"username"`
	MobileNumber    string     `json:"mobile_number"`
	Email           string     `json:"email,omitempty"`
	PlanID          int        `json:"plan_id"`
	FranchiseID     *int       `json:"franchise_id,omitempty"`
	Status          string     `json:"status"`
	DunningState    string     `json:"dunning_state"`
	WalletBalance   string     `json:"wallet_balance"`
	RegisteredState string     `json:"registered_state"`
	KYCStatus       string     `json:"kyc_status"`
	PlanExpiry      *time.Time `json:"plan_expiry,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// CreateSubscriberRequest is the POST /api/v1/subscribers body.
type CreateSubscriberRequest struct {
	CAFNumber       string `json:"caf_number"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	MobileNumber    string `json:"mobile_number"`
	Email           string `json:"email"`
	PlanID          int    `json:"plan_id"`
	RegisteredState string `json:"registered_state"`
	Aadhaar         string `json:"aadhaar,omitempty"`
	PAN             string `json:"pan,omitempty"`
}

// SubscriberQuerier is the DB interface for subscriber operations.
type SubscriberQuerier interface {
	CreateSubscriber(ctx context.Context, sub SubscriberRecord, passwordHash string) (*SubscriberRecord, error)
	GetSubscriberByID(ctx context.Context, id int) (*SubscriberRecord, error)
	UpdateSubscriber(ctx context.Context, id int, planID *int, status *string) (*SubscriberRecord, error)
	GetSubscriberByUsername(ctx context.Context, username string) (*SubscriberRecord, error)
}

// KYCQuerier handles KYC persistence.
type KYCQuerier interface {
	UpsertKYC(ctx context.Context, subscriberID int, aadhaarEnc, panEnc, keyVersion string) error
}

// Handler holds all API route dependencies.
type Handler struct {
	db        SubscriberQuerier
	kycDB     KYCQuerier
	walletSvc *billing.WalletService
	keyStore  crypto.KeyStore

	ledger     LedgerQuerier
	sessions   SessionReader
	sessionCtl SessionController
	tasks      TaskEnqueuer
	invoices   InvoiceQuerier
	pdfGen     PDFGenerator
	tickets    TicketAdminQuerier
	lea        LEAQuerier
	leaAudit   LEAAuditRecorder

	razorpayWebhookSecret string
}

// HandlerDeps bundles every Handler dependency.
//
// A plain multi-argument constructor stopped being readable once the wiring
// grew past the original four collaborators (db, kycDB, walletSvc, keyStore);
// a struct makes each dependency's purpose explicit at the call site and lets
// optional ones (Sessions, Invoices, LEA, ...) be left as their zero value
// without every caller having to pass a run of nils in the right order.
//
// A nil optional dependency is not a startup error: each handler that needs it
// checks and returns 503 rather than panicking, so a deployment that has not
// configured (say) Gotenberg still serves every other route.
type HandlerDeps struct {
	DB       SubscriberQuerier
	KYC      KYCQuerier
	Wallet   *billing.WalletService
	KeyStore crypto.KeyStore

	Ledger     LedgerQuerier
	Sessions   SessionReader
	SessionCtl SessionController
	Tasks      TaskEnqueuer
	Invoices   InvoiceQuerier
	PDF        PDFGenerator
	Tickets    TicketAdminQuerier
	LEA        LEAQuerier
	LEAAudit   LEAAuditRecorder

	RazorpayWebhookSecret string
}

// NewHandler constructs the API Handler.
func NewHandler(deps HandlerDeps) *Handler {
	return &Handler{
		db:        deps.DB,
		kycDB:     deps.KYC,
		walletSvc: deps.Wallet,
		keyStore:  deps.KeyStore,

		ledger:     deps.Ledger,
		sessions:   deps.Sessions,
		sessionCtl: deps.SessionCtl,
		tasks:      deps.Tasks,
		invoices:   deps.Invoices,
		pdfGen:     deps.PDF,
		tickets:    deps.Tickets,
		lea:        deps.LEA,
		leaAudit:   deps.LEAAudit,

		razorpayWebhookSecret: deps.RazorpayWebhookSecret,
	}
}

// RegisterRoutes wires all API routes onto the provided mux using Go 1.21 pattern syntax.
//
// API §7 | FR: API-003, API-004
func (h *Handler) RegisterRoutes(mux *http.ServeMux, jwtSecret string) {
	auth := middleware.JWTMiddleware(jwtSecret)
	admin := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("billing_admin", "isp_owner")(next))
	}
	staffRead := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("noc_engineer", "billing_admin", "csr", "technician", "isp_owner")(next))
	}
	nocOnly := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("noc_engineer", "isp_owner")(next))
	}
	// LEA export requires the noc_engineer role AND the separate lea_access
	// claim (SecD §9.3 "noc + lea_flag"): the two are independent grants, so a
	// noc_engineer token minted without lea_access must not reach this route.
	nocWithLea := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("noc_engineer", "isp_owner")(middleware.RequireLeaAccess(next)))
	}
	billingOrCSR := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("billing_admin", "csr", "isp_owner")(next))
	}
	csrOrTech := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("csr", "technician", "isp_owner")(next))
	}

	// Health (no auth)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Subscribers CRUD (API-003)
	mux.Handle("POST /api/v1/subscribers",
		admin(http.HandlerFunc(h.CreateSubscriber)))
	mux.Handle("GET /api/v1/subscribers/{id}",
		staffRead(http.HandlerFunc(h.GetSubscriber)))
	mux.Handle("PATCH /api/v1/subscribers/{id}",
		admin(http.HandlerFunc(h.UpdateSubscriber)))
	mux.Handle("GET /api/v1/subscribers/{id}/health",
		staffRead(http.HandlerFunc(h.GetSubscriberHealth)))

	// Wallets (API-003)
	mux.Handle("POST /api/v1/wallets/recharge",
		admin(http.HandlerFunc(h.WalletRecharge)))
	mux.Handle("GET /api/v1/wallets/{subscriber_id}/ledger",
		billingOrCSR(http.HandlerFunc(h.GetLedger)))

	// Sessions (API-004)
	mux.Handle("GET /api/v1/sessions/{subscriber_id}/active",
		staffRead(http.HandlerFunc(h.GetActiveSession)))
	mux.Handle("POST /api/v1/sessions/{session_id}/disconnect",
		nocOnly(http.HandlerFunc(h.DisconnectSession)))
	mux.Handle("POST /api/v1/sessions/{session_id}/fup-override",
		nocOnly(http.HandlerFunc(h.FUPOverride)))

	// Invoices (API-004)
	mux.Handle("GET /api/v1/invoices/{subscriber_id}",
		billingOrCSR(http.HandlerFunc(h.ListInvoices)))
	mux.Handle("GET /api/v1/invoices/{invoice_id}/pdf",
		billingOrCSR(http.HandlerFunc(h.GetInvoicePDF)))

	// Tickets (API-004)
	mux.Handle("POST /api/v1/tickets",
		billingOrCSR(http.HandlerFunc(h.CreateTicket)))
	mux.Handle("PATCH /api/v1/tickets/{ticket_id}",
		csrOrTech(http.HandlerFunc(h.UpdateTicket)))

	// LEA (API-004)
	mux.Handle("POST /api/v1/lea/lookup",
		nocWithLea(http.HandlerFunc(h.LEALookup)))

	// Webhooks (no JWT — uses HMAC)
	mux.HandleFunc("POST /webhooks/razorpay",
		h.RazorpayWebhook)
}

// ── Subscribers ──────────────────────────────────────────────────────────────

// CreateSubscriber handles POST /api/v1/subscribers.
// Hashes password, encrypts PII, persists subscriber.
func (h *Handler) CreateSubscriber(w http.ResponseWriter, r *http.Request) {
	var req CreateSubscriberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if err := validateCreateSubscriber(req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "password hash failed")
		return
	}

	rec := SubscriberRecord{
		CAFNumber:       req.CAFNumber,
		Username:        req.Username,
		MobileNumber:    req.MobileNumber,
		Email:           req.Email,
		PlanID:          req.PlanID,
		RegisteredState: req.RegisteredState,
		Status:          "active",
		DunningState:    "active",
		KYCStatus:       "pending",
		WalletBalance:   "0.00",
	}

	created, err := h.db.CreateSubscriber(r.Context(), rec, string(hash))
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "ERR_CONFLICT", "CAF number or username already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create subscriber failed")
		return
	}

	// Encrypt PII if provided
	if (req.Aadhaar != "" || req.PAN != "") && h.keyStore != nil {
		enc, err := crypto.NewAESEncryptor(h.keyStore)
		if err == nil {
			aadhaarEnc, panEnc := "", ""
			if req.Aadhaar != "" {
				aadhaarEnc, _ = enc.Encrypt(req.Aadhaar)
			}
			if req.PAN != "" {
				panEnc, _ = enc.Encrypt(req.PAN)
			}
			if err := h.kycDB.UpsertKYC(r.Context(), created.ID, aadhaarEnc, panEnc, h.keyStore.ActiveVersion()); err != nil {
				log.Warn().Err(err).Msg("api: KYC persist failed; subscriber created without KYC")
			}
		}
	}

	middleware.Audit(r.Context(), "subscriber.create", strconv.Itoa(created.ID), nil)
	writeJSON(w, http.StatusCreated, created)
}

// GetSubscriber handles GET /api/v1/subscribers/{id}.
func (h *Handler) GetSubscriber(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	sub, err := h.db.GetSubscriberByID(r.Context(), id)
	if err != nil || sub == nil {
		writeError(w, http.StatusNotFound, "ERR_SUBSCRIBER_NOT_FOUND",
			fmt.Sprintf("Subscriber with ID %d not found.", id))
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

// UpdateSubscriber handles PATCH /api/v1/subscribers/{id}.
func (h *Handler) UpdateSubscriber(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var body struct {
		PlanID *int    `json:"plan_id"`
		Status *string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	updated, err := h.db.UpdateSubscriber(r.Context(), id, body.PlanID, body.Status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "update failed")
		return
	}
	middleware.Audit(r.Context(), "subscriber.update", strconv.Itoa(id), map[string]any{
		"plan_id": body.PlanID, "status": body.Status,
	})
	writeJSON(w, http.StatusOK, updated)
}

// GetSubscriberHealth handles GET /api/v1/subscribers/{id}/health — implemented in health package.
func (h *Handler) GetSubscriberHealth(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "delegate to health.Handler.GetSubscriberHealth", http.StatusNotImplemented)
}

// ── Wallets ──────────────────────────────────────────────────────────────────

// WalletRecharge handles POST /api/v1/wallets/recharge.
func (h *Handler) WalletRecharge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SubscriberID     int    `json:"subscriber_id"`
		Amount           string `json:"amount"`
		PaymentMethod    string `json:"payment_method"`
		TransactionToken string `json:"transaction_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "invalid amount")
		return
	}
	tx, err := h.walletSvc.Recharge(r.Context(), billing.RechargeRequest{
		SubscriberID:     req.SubscriberID,
		Amount:           amount,
		TransactionToken: req.TransactionToken,
		Description:      "recharge via " + req.PaymentMethod,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		return
	}
	middleware.Audit(r.Context(), "wallet.recharge", strconv.Itoa(req.SubscriberID), map[string]any{
		"amount": req.Amount, "method": req.PaymentMethod,
	})
	writeJSON(w, http.StatusOK, tx)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"code": errCode, "message": msg})
}

func pathInt(r *http.Request, key string) (int, error) {
	return strconv.Atoi(r.PathValue(key))
}

func isUniqueViolation(err error) bool {
	return err != nil && len(err.Error()) > 0 &&
		(contains(err.Error(), "unique") || contains(err.Error(), "duplicate"))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := range s {
		if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func validateCreateSubscriber(req CreateSubscriberRequest) error {
	if req.CAFNumber == "" {
		return fmt.Errorf("caf_number is required")
	}
	if req.Username == "" {
		return fmt.Errorf("username is required")
	}
	if req.MobileNumber == "" {
		return fmt.Errorf("mobile_number is required")
	}
	if req.PlanID == 0 {
		return fmt.Errorf("plan_id is required")
	}
	if req.RegisteredState == "" {
		return fmt.Errorf("registered_state is required")
	}
	return nil
}
