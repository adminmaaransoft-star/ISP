package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hibiken/asynq"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Subscriber lifecycle endpoints — FR-BIL-008..011, FR-LC-001..003 | MDS §4.14.
//
// Plan change and termination are deliberately separate endpoints from
// PATCH /subscribers/{id} rather than extensions of it: PATCH already has
// call sites that expect a bare field write with no side effects, and
// overloading it to sometimes prorate, invalidate cache and fire a CoA/PoD —
// based on which field changed — would make its behavior conditional on
// intent invisible in the request body.

// ErrInvalidPlan is returned by LifecycleQuerier.GetPlanChangeInfo when the
// requested new_plan_id does not exist.
var ErrInvalidPlan = errors.New("api: unknown plan id")

// PlanChangeInfo carries what plan-change proration needs: both plans'
// price/validity and the subscriber's current plan_expiry.
type PlanChangeInfo struct {
	Username        string
	CurrentExpiry   *time.Time
	OldPrice        decimal.Decimal
	OldValidityDays int
	NewPrice        decimal.Decimal
	NewValidityDays int
}

// LifecycleQuerier is the DB surface subscriber lifecycle actions need.
// Satisfied by *db.APIStore.
type LifecycleQuerier interface {
	// GetPlanChangeInfo returns (nil, nil) for an unknown subscriber, and
	// (nil, ErrInvalidPlan) for an unknown newPlanID.
	GetPlanChangeInfo(ctx context.Context, subscriberID, newPlanID int) (*PlanChangeInfo, error)
	SetSubscriberPlan(ctx context.Context, subscriberID, newPlanID int, newExpiry time.Time) (*SubscriberRecord, error)
	TerminateSubscriber(ctx context.Context, subscriberID int) (*SubscriberRecord, error)
}

// RefundQuerier persists a staff-issued refund's business record. Satisfied
// by *db.BillingStore.
type RefundQuerier interface {
	CreateRefund(ctx context.Context, subscriberID, ledgerEntryID int, amount decimal.Decimal, reason, refundedBy string) (int, error)
}

// SubscriberCacheInvalidator drops a cached auth-cache record so the next
// authentication reloads plan/status changes. Satisfied by
// *cache.SubscriberCache.
type SubscriberCacheInvalidator interface {
	InvalidateSubscriber(ctx context.Context, username string) error
}

// ── FR-LC-001: Plan change ──────────────────────────────────────────────────

type planChangeRequest struct {
	NewPlanID int `json:"new_plan_id"`
}

// ChangeSubscriberPlan handles POST /api/v1/subscribers/{id}/plan-change.
//
// Prorates the unused value of the old plan into bonus days on the new one,
// invalidates the Redis auth-cache entry, and enqueues a CoA to any active
// session — closing FR-AAA-007, which was specified but never implemented.
//
// FR: FR-LC-001 | MDS §4.14
func (h *Handler) ChangeSubscriberPlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.lifecycle == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lifecycle store not configured")
		return
	}

	var req planChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.NewPlanID <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "new_plan_id is required")
		return
	}

	info, err := h.lifecycle.GetPlanChangeInfo(r.Context(), id, req.NewPlanID)
	if errors.Is(err, ErrInvalidPlan) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "new_plan_id does not exist")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "plan change lookup failed")
		return
	}
	if info == nil {
		writeError(w, http.StatusNotFound, "ERR_SUBSCRIBER_NOT_FOUND", "subscriber not found")
		return
	}

	newExpiry := computePlanChangeExpiry(info, time.Now())

	updated, err := h.lifecycle.SetSubscriberPlan(r.Context(), id, req.NewPlanID, newExpiry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "plan change failed")
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "ERR_SUBSCRIBER_NOT_FOUND", "subscriber not found")
		return
	}

	if h.subCache != nil {
		if err := h.subCache.InvalidateSubscriber(r.Context(), info.Username); err != nil {
			log.Error().Err(err).Int("subscriber_id", id).Msg("api: auth-cache invalidation failed after plan change")
		}
	}
	enqueueCoAIfSessionActive(r.Context(), h, id)

	billing.LifecycleActionsTotal.WithLabelValues("plan_change").Inc()
	middleware.Audit(r.Context(), "subscriber.plan_change", strconv.Itoa(id), map[string]any{
		"new_plan_id": req.NewPlanID, "new_plan_expiry": newExpiry,
	})
	writeJSON(w, http.StatusOK, updated)
}

// computePlanChangeExpiry applies the proration rule from MDS §4.14: the
// subscriber always gets the new plan's full validity from now, plus bonus
// days converted from whatever value remained unused on the old plan.
//
// All money math stays in decimal.Decimal — remaining validity is converted
// from a time.Duration via integer nanoseconds rather than float division,
// so day-fraction rounding never enters a currency computation (FR-BIL-002).
func computePlanChangeExpiry(info *PlanChangeInfo, now time.Time) time.Time {
	remaining := time.Duration(0)
	if info.CurrentExpiry != nil {
		if d := info.CurrentExpiry.Sub(now); d > 0 {
			remaining = d
		}
	}
	remainingDays := decimal.NewFromInt(int64(remaining)).Div(decimal.NewFromInt(int64(24 * time.Hour)))

	bonusDays := 0
	if info.OldValidityDays > 0 && info.NewValidityDays > 0 && info.NewPrice.IsPositive() {
		oldDaily := info.OldPrice.Div(decimal.NewFromInt(int64(info.OldValidityDays)))
		credit := oldDaily.Mul(remainingDays)
		newDaily := info.NewPrice.Div(decimal.NewFromInt(int64(info.NewValidityDays)))
		bonusDays = int(credit.Div(newDaily).Floor().IntPart())
	}

	return now.AddDate(0, 0, info.NewValidityDays+bonusDays)
}

// ── FR-LC-002: Termination ──────────────────────────────────────────────────

// TerminateSubscriber handles POST /api/v1/subscribers/{id}/terminate.
//
// Sets status to terminated and enqueues a PoD to any active session —
// distinct from suspension, which only throttles. Irreversible: there is no
// "un-terminate" action.
//
// FR: FR-LC-002 | MDS §4.14
func (h *Handler) TerminateSubscriber(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.lifecycle == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lifecycle store not configured")
		return
	}

	updated, err := h.lifecycle.TerminateSubscriber(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "termination failed")
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "ERR_SUBSCRIBER_NOT_FOUND", "subscriber not found")
		return
	}

	if h.subCache != nil {
		if err := h.subCache.InvalidateSubscriber(r.Context(), updated.Username); err != nil {
			log.Error().Err(err).Int("subscriber_id", id).Msg("api: auth-cache invalidation failed after termination")
		}
	}
	enqueuePoDIfSessionActive(r.Context(), h, id)

	billing.LifecycleActionsTotal.WithLabelValues("terminate").Inc()
	middleware.Audit(r.Context(), "subscriber.terminate", strconv.Itoa(id), nil)
	writeJSON(w, http.StatusOK, updated)
}

// ── FR-BIL-010: Staff adjustments ───────────────────────────────────────────

type adjustmentRequest struct {
	Amount    string `json:"amount"`
	Direction string `json:"direction"` // "credit" | "debit"
	Reason    string `json:"reason"`
}

// CreateAdjustment handles POST /api/v1/subscribers/{id}/adjustments.
//
// FR: FR-BIL-010 | MDS §4.14
func (h *Handler) CreateAdjustment(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.walletSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "wallet service not configured")
		return
	}

	var req adjustmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Direction != "credit" && req.Direction != "debit" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "direction must be \"credit\" or \"debit\"")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "reason is required")
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "amount must be a positive decimal")
		return
	}

	staffUsername := middleware.SubjectFromContext(r.Context())
	tx, err := h.walletSvc.Post(r.Context(), billing.PostRequest{
		SubscriberID:   id,
		Amount:         amount,
		Direction:      req.Direction,
		CounterAccount: billing.AccountAdjustmentClearing,
		AdjustedBy:     staffUsername,
		Description:    "staff adjustment: " + req.Reason,
	})
	if errors.Is(err, billing.ErrInsufficientBalance) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "debit exceeds current wallet balance")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "adjustment failed")
		return
	}

	billing.LifecycleActionsTotal.WithLabelValues("adjustment").Inc()
	middleware.Audit(r.Context(), "subscriber.adjustment", strconv.Itoa(id), map[string]any{
		"amount": amount.StringFixed(2), "direction": req.Direction, "reason": req.Reason,
	})
	writeJSON(w, http.StatusCreated, map[string]string{
		"transaction_id": strconv.Itoa(tx.ID), "wallet_balance": tx.BalanceAfter.StringFixed(2),
	})
}

// ── FR-BIL-011: Refunds ─────────────────────────────────────────────────────

type refundRequest struct {
	Amount string `json:"amount"`
	Reason string `json:"reason"`
}

// CreateRefund handles POST /api/v1/subscribers/{id}/refunds.
//
// FR: FR-BIL-011 | MDS §4.14
func (h *Handler) CreateRefund(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.walletSvc == nil || h.refunds == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "refund processing not configured")
		return
	}

	var req refundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "reason is required")
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "amount must be a positive decimal")
		return
	}

	staffUsername := middleware.SubjectFromContext(r.Context())
	tx, err := h.walletSvc.Post(r.Context(), billing.PostRequest{
		SubscriberID:   id,
		Amount:         amount,
		Direction:      "debit",
		CounterAccount: billing.AccountAdjustmentClearing,
		AdjustedBy:     staffUsername,
		Description:    "refund: " + req.Reason,
	})
	if errors.Is(err, billing.ErrInsufficientBalance) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "cannot refund more than the current wallet balance")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "refund failed")
		return
	}

	refundID, err := h.refunds.CreateRefund(r.Context(), id, tx.ID, amount, req.Reason, staffUsername)
	if err != nil {
		// The wallet debit above already committed and must not be undone
		// just because the refund record failed — the ledger leg is itself
		// a true and complete record of the money movement; log for
		// reconciliation rather than leaving a refunded subscriber's wallet
		// debited with no record, matching the same "money moved, log the
		// rest" pattern renewalProcessor.ApplyRenewal uses (cmd/api/main.go).
		log.Error().Err(err).Int("subscriber_id", id).Int("ledger_entry_id", tx.ID).
			Msg("api: refund record failed after wallet debit committed")
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "refund debit succeeded but the refund record failed; contact support")
		return
	}

	billing.LifecycleActionsTotal.WithLabelValues("refund").Inc()
	middleware.Audit(r.Context(), "subscriber.refund", strconv.Itoa(id), map[string]any{
		"amount": amount.StringFixed(2), "reason": req.Reason, "refund_id": refundID,
	})
	writeJSON(w, http.StatusCreated, map[string]string{
		"refund_id": strconv.Itoa(refundID), "transaction_id": strconv.Itoa(tx.ID), "wallet_balance": tx.BalanceAfter.StringFixed(2),
	})
}

// ── Session-control helpers ─────────────────────────────────────────────────

const lifecycleTaskRetention = 24 * time.Hour

// enqueueCoAIfSessionActive mirrors FUPOverride's CoA-enqueue pattern
// (internal/api/sessions.go), but starting from a subscriber_id rather than
// a session_id: it looks up whether a session is active first, since a
// plan-change endpoint has no session_id in its URL. A subscriber with no
// active session needs no CoA — their next login already sees the new plan.
func enqueueCoAIfSessionActive(ctx context.Context, h *Handler, subscriberID int) {
	if h.sessions == nil || h.tasks == nil {
		return
	}
	sess, err := h.sessions.GetActiveSession(ctx, subscriberID)
	if err != nil || sess == nil {
		return
	}
	payload, _ := json.Marshal(fup.CoAPayload{SubscriberID: subscriberID, NasIP: sess.NasIP}) //nolint:errcheck
	task := asynq.NewTask(fup.TaskTypeCoA, payload,
		asynq.Queue(fup.QueueNetCommands), asynq.MaxRetry(5), asynq.Retention(lifecycleTaskRetention))
	if _, err := h.tasks.Enqueue(task); err != nil {
		log.Error().Err(err).Int("subscriber_id", subscriberID).Msg("api: enqueue CoA task failed")
	}
}

// enqueuePoDIfSessionActive mirrors DisconnectSession's PoD-enqueue pattern,
// starting from a subscriber_id. A subscriber with no active session has
// nothing to disconnect.
func enqueuePoDIfSessionActive(ctx context.Context, h *Handler, subscriberID int) {
	if h.sessions == nil || h.tasks == nil {
		return
	}
	sess, err := h.sessions.GetActiveSession(ctx, subscriberID)
	if err != nil || sess == nil {
		return
	}
	payload, _ := json.Marshal(fup.PoDPayload{SubscriberID: subscriberID}) //nolint:errcheck
	task := asynq.NewTask(fup.TaskTypePoD, payload,
		asynq.Queue(fup.QueueNetCommands), asynq.MaxRetry(5), asynq.Retention(lifecycleTaskRetention))
	if _, err := h.tasks.Enqueue(task); err != nil {
		log.Error().Err(err).Int("subscriber_id", subscriberID).Msg("api: enqueue PoD task failed")
	}
}
