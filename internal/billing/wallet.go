package billing

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"
)

// Ledger account labels for the two legs a recharge posts.
const (
	AccountSubscriberWallet = "subscriber_wallet"
	AccountGatewayClearing  = "payment_gateway_clearing"
)

// WalletQuerier is the DB interface required by WalletService.
type WalletQuerier interface {
	GetTransactionByToken(ctx context.Context, token string) (*Transaction, error)
	// RecordRecharge must persist both ledger legs and the new wallet balance in
	// a single DB transaction. Splitting them would let a crash between the two
	// leave the ledger and subscribers.wallet_balance permanently disagreeing,
	// which the nightly reconciliation (FR-REV-002) would then report as variance.
	RecordRecharge(ctx context.Context, p RechargePosting) (*Transaction, error)
	GetSubscriberBalance(ctx context.Context, subscriberID int) (decimal.Decimal, error)
}

// Transaction represents a completed wallet ledger entry.
type Transaction struct {
	ID               int
	SubscriberID     int
	EntryType        string
	Amount           decimal.Decimal
	BalanceAfter     decimal.Decimal
	TransactionToken string
	Description      string
}

// WalletEntry is one leg of a ledger posting.
type WalletEntry struct {
	SubscriberID     int
	FranchiseID      *int
	Account          string // subscriber_wallet | payment_gateway_clearing
	EntryType        string // credit | debit
	Amount           decimal.Decimal
	BalanceAfter     decimal.Decimal
	TransactionToken *string // nil = no idempotency key (cash, or counter-leg)
	Description      string
}

// RechargePosting is the atomic unit a recharge writes: both ledger legs plus
// the resulting wallet balance.
type RechargePosting struct {
	SubscriberID int
	Debit        WalletEntry
	Credit       WalletEntry
	NewBalance   decimal.Decimal
}

// RechargeRequest carries the inputs for a subscriber wallet top-up.
type RechargeRequest struct {
	SubscriberID     int
	Amount           decimal.Decimal
	TransactionToken string // Razorpay payment_id or equivalent
	FranchiseID      *int
	Description      string
}

// WalletService performs double-entry wallet operations with idempotency.
type WalletService struct {
	db WalletQuerier
}

// NewWalletService constructs a WalletService.
func NewWalletService(db WalletQuerier) *WalletService {
	return &WalletService{db: db}
}

// Recharge credits a subscriber's wallet, posting both legs of the double entry.
// Idempotent: a second call with the same TransactionToken returns the original
// transaction without moving money again.
//
// FR: FR-BIL-003, FR-BIL-005 | DDS §5.6
func (s *WalletService) Recharge(ctx context.Context, req RechargeRequest) (*Transaction, error) {
	if req.Amount.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("billing: recharge amount must be positive, got %s", req.Amount)
	}

	// Idempotency check — if the token already exists, return the original transaction
	if req.TransactionToken != "" {
		existing, err := s.db.GetTransactionByToken(ctx, req.TransactionToken)
		if err == nil && existing != nil {
			return existing, nil
		}
	}

	currentBalance, err := s.db.GetSubscriberBalance(ctx, req.SubscriberID)
	if err != nil {
		return nil, fmt.Errorf("billing: get balance: %w", err)
	}
	newBalance := currentBalance.Add(req.Amount)

	// Only the credit leg carries the idempotency token: wallet_ledgers has a
	// unique index on transaction_token, so both legs holding it would collide.
	var tokenPtr *string
	if req.TransactionToken != "" {
		t := req.TransactionToken
		tokenPtr = &t
	}

	posting := RechargePosting{
		SubscriberID: req.SubscriberID,
		Debit: WalletEntry{
			SubscriberID: req.SubscriberID,
			FranchiseID:  req.FranchiseID,
			Account:      AccountGatewayClearing,
			EntryType:    "debit",
			Amount:       req.Amount,
			BalanceAfter: newBalance,
			Description:  "counter-entry: " + req.Description,
		},
		Credit: WalletEntry{
			SubscriberID:     req.SubscriberID,
			FranchiseID:      req.FranchiseID,
			Account:          AccountSubscriberWallet,
			EntryType:        "credit",
			Amount:           req.Amount,
			BalanceAfter:     newBalance,
			TransactionToken: tokenPtr,
			Description:      req.Description,
		},
		NewBalance: newBalance,
	}

	tx, err := s.db.RecordRecharge(ctx, posting)
	if err != nil {
		return nil, fmt.Errorf("billing: record recharge: %w", err)
	}
	tx.BalanceAfter = newBalance
	return tx, nil
}
