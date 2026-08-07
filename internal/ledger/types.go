// Package ledger is the core of ForgeRail: a double-entry ledger for USDC.
//
// Every movement of money is a Transfer. A transfer posts one or more
// balanced pairs of Entries (one debit, one credit) and each pair is tagged
// with a Phase. The (transfer, phase) pair is unique in storage, which is what
// makes retries safe: posting the same phase twice is a no-op.
package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Amaan729/ForgeRail/internal/money"
)

// System accounts. These are created by the first migration.
const (
	// AccountOnchainIn is debited when USDC arrives from outside (deposits),
	// so it is allowed to go negative.
	AccountOnchainIn = "sys_onchain_in"
	// AccountSettlement holds funds for withdrawals that are in flight.
	AccountSettlement = "sys_settlement"
	// AccountOnchainOut is credited once a withdrawal is confirmed on Base.
	AccountOnchainOut = "sys_onchain_out"
)

var (
	ErrNotFound            = errors.New("ledger: not found")
	ErrAccountExists       = errors.New("ledger: account already exists")
	ErrInvalidRequest      = errors.New("ledger: invalid request")
	ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with a different request")
	ErrInvalidState        = errors.New("ledger: transfer is not in a valid state for this operation")
)

// FailureInsufficientFunds is recorded on transfers rejected for lack of funds.
const FailureInsufficientFunds = "insufficient_funds"

type Account struct {
	ID            string
	Name          string
	Balance       money.Amount
	AllowNegative bool
	CreatedAt     time.Time
}

type NewAccount struct {
	ID            string // optional, generated when empty
	Name          string
	AllowNegative bool
}

type TransferKind string

const (
	KindInternal   TransferKind = "internal"   // account -> account, posted immediately
	KindWithdrawal TransferKind = "withdrawal" // account -> address on Base, settled async
)

type TransferStatus string

const (
	StatusPosted    TransferStatus = "posted"    // internal transfer applied
	StatusPending   TransferStatus = "pending"   // withdrawal: funds held
	StatusSubmitted TransferStatus = "submitted" // withdrawal: tx broadcast to Base
	StatusSettled   TransferStatus = "settled"   // withdrawal: confirmed on chain
	StatusFailed    TransferStatus = "failed"    // withdrawal: hold released
	StatusRejected  TransferStatus = "rejected"  // nothing was posted
)

// Terminal reports whether the status can no longer change.
func (s TransferStatus) Terminal() bool {
	switch s {
	case StatusPosted, StatusSettled, StatusFailed, StatusRejected:
		return true
	}
	return false
}

type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

type Phase string

const (
	PhasePost    Phase = "post"    // internal transfer
	PhaseHold    Phase = "hold"    // withdrawal: user -> settlement
	PhaseSettle  Phase = "settle"  // withdrawal: settlement -> onchain_out
	PhaseRelease Phase = "release" // withdrawal: settlement -> user
)

type Transfer struct {
	ID             string
	IdempotencyKey string
	RequestHash    string
	Kind           TransferKind
	FromAccount    string
	ToAccount      string // internal transfers
	Destination    string // withdrawals: 0x address on Base
	Amount         money.Amount
	Memo           string
	Status         TransferStatus
	FailureReason  string
	TxHash         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Entry struct {
	ID         int64
	TransferID string
	AccountID  string
	Direction  Direction
	Amount     money.Amount
	Phase      Phase
	CreatedAt  time.Time
}

type TransferRequest struct {
	IdempotencyKey string
	Kind           TransferKind
	FromAccount    string
	ToAccount      string
	Destination    string
	Amount         money.Amount
	Memo           string
}

var addressRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// Validate checks the request shape. It does not touch balances.
func (r TransferRequest) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidRequest, msg) }
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return bad("idempotency key is required")
	}
	if len(r.IdempotencyKey) > 255 {
		return bad("idempotency key is too long")
	}
	if r.Amount <= 0 {
		return bad("amount must be positive")
	}
	if r.FromAccount == "" {
		return bad("from account is required")
	}
	if len(r.Memo) > 500 {
		return bad("memo is too long")
	}
	switch r.Kind {
	case KindInternal:
		if r.ToAccount == "" {
			return bad("to account is required")
		}
		if r.ToAccount == r.FromAccount {
			return bad("cannot transfer to the same account")
		}
		if r.Destination != "" {
			return bad("destination is only valid for withdrawals")
		}
	case KindWithdrawal:
		if !addressRE.MatchString(r.Destination) {
			return bad("destination must be a 0x-prefixed 20 byte address")
		}
		if r.ToAccount != "" {
			return bad("to account is not valid for withdrawals")
		}
	default:
		return bad("unknown transfer kind")
	}
	return nil
}

// Hash fingerprints everything except the idempotency key, so a retry with
// the same key but a different body can be detected and refused.
func (r TransferRequest) Hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\x00%s",
		r.Kind, r.FromAccount, r.ToAccount, strings.ToLower(r.Destination), r.Amount.Micros(), r.Memo)
	return hex.EncodeToString(h.Sum(nil))
}
