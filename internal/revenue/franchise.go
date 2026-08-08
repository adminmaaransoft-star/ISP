package revenue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// FranchiseQuerier is the DB interface for franchise isolation and commission.
type FranchiseQuerier interface {
	GetFranchiseByID(ctx context.Context, franchiseID int) (*Franchise, error)
	CalculateAndStoreLCOCommission(ctx context.Context, entry LCOCommissionEntry) error
}

// Franchise represents a franchise record.
type Franchise struct {
	ID                int
	Name              string
	CommissionRatePct decimal.Decimal
	Status            string
}

// LCOCommissionEntry is one lco_ledger row: the recharge that earned the
// commission plus the computed commission itself.
type LCOCommissionEntry struct {
	FranchiseID      int
	SubscriberID     int
	RechargeAmount   decimal.Decimal
	CommissionRate   decimal.Decimal
	CommissionAmount decimal.Decimal
	TransactionRef   string
}

// CalculateLCOCommission computes and persists the franchise commission for a recharge.
//
// FR: FR-FRN-002 | DBD §6.2 lco_ledger
func CalculateLCOCommission(ctx context.Context, db FranchiseQuerier, entry LCOCommissionEntry) (decimal.Decimal, error) {
	franchise, err := db.GetFranchiseByID(ctx, entry.FranchiseID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("revenue: get franchise %d: %w", entry.FranchiseID, err)
	}
	if franchise.Status != "active" {
		return decimal.Zero, fmt.Errorf("revenue: franchise %d is not active", entry.FranchiseID)
	}

	commission := entry.RechargeAmount.
		Mul(franchise.CommissionRatePct).
		Div(decimal.NewFromInt(100)).
		Round(2)

	ledgerRow := LCOCommissionEntry{
		FranchiseID:      entry.FranchiseID,
		SubscriberID:     entry.SubscriberID,
		RechargeAmount:   entry.RechargeAmount,
		CommissionRate:   franchise.CommissionRatePct,
		CommissionAmount: commission,
		TransactionRef:   entry.TransactionRef,
	}
	if err := db.CalculateAndStoreLCOCommission(ctx, ledgerRow); err != nil {
		return decimal.Zero, fmt.Errorf("revenue: store lco commission: %w", err)
	}
	return commission, nil
}

// franchiseScopedRoles are the roles whose visibility is confined to a single
// franchise. Every other role is either ISP-wide staff or has no data access.
var franchiseScopedRoles = map[string]bool{
	"lco":             true,
	"franchise_admin": true,
	"franchise_staff": true,
}

// Scope carries the franchise restriction resolved for a request.
// A nil FranchiseID means unrestricted (ISP-wide) visibility.
type Scope struct {
	FranchiseID *int
}

type scopeCtxKey struct{}

// FranchiseMiddleware resolves the franchise restriction for the caller and
// injects it into the request context, so downstream queries cannot accidentally
// read across franchise boundaries.
//
// A franchise-scoped role presenting a token without a franchise_id is rejected
// rather than silently granted ISP-wide visibility.
//
// FR: FR-FRN-001 | DDS §5.7
func FranchiseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := middleware.RoleFromContext(r.Context())
		franchiseID := middleware.FranchiseIDFromContext(r.Context())

		scope := Scope{}
		if franchiseScopedRoles[role] {
			if franchiseID == 0 {
				http.Error(w, "forbidden: token has no franchise binding", http.StatusForbidden)
				return
			}
			id := franchiseID
			scope.FranchiseID = &id
		}

		ctx := context.WithValue(r.Context(), scopeCtxKey{}, scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ScopeFromContext returns the franchise scope injected by FranchiseMiddleware.
func ScopeFromContext(ctx context.Context) Scope {
	scope, _ := ctx.Value(scopeCtxKey{}).(Scope)
	return scope
}

// SubscriberRow is the franchise-scoped subscriber projection.
type SubscriberRow struct {
	ID          int    `json:"id"`
	Username    string `json:"username"`
	FranchiseID *int   `json:"franchise_id,omitempty"`
}

// SubscriberLister returns subscribers visible within a franchise scope.
// A nil franchiseID must return every subscriber.
type SubscriberLister interface {
	ListSubscribers(ctx context.Context, franchiseID *int) ([]SubscriberRow, error)
}

// ListSubscribersHandler serves GET /api/v1/subscribers scoped to the caller's
// franchise. It must be mounted behind FranchiseMiddleware.
//
// FR: FR-FRN-001
func ListSubscribersHandler(db SubscriberLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := ScopeFromContext(r.Context())
		rows, err := db.ListSubscribers(r.Context(), scope.FranchiseID)
		if err != nil {
			http.Error(w, "failed to list subscribers", http.StatusInternalServerError)
			return
		}
		if rows == nil {
			rows = []SubscriberRow{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows) //nolint:errcheck
	}
}
