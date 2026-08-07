package ledger

import "context"

// Store is implemented by the in-memory store (tests, local dev) and the
// Postgres store. Both must pass the suite in ledgertest.
//
// Every write is idempotent:
//   - CreateTransfer is keyed by IdempotencyKey. Replaying a key returns the
//     original transfer with replayed=true and posts nothing new.
//   - MarkSubmitted, Settle and Release can be called any number of times;
//     only the first call that changes state posts entries.
type Store interface {
	CreateAccount(ctx context.Context, a NewAccount) (Account, error)
	GetAccount(ctx context.Context, id string) (Account, error)

	CreateTransfer(ctx context.Context, req TransferRequest) (t Transfer, replayed bool, err error)
	GetTransfer(ctx context.Context, id string) (Transfer, error)
	ListEntries(ctx context.Context, accountID string, limit int) ([]Entry, error)

	// Withdrawal lifecycle.
	MarkSubmitted(ctx context.Context, transferID, txHash string) (Transfer, error)
	Settle(ctx context.Context, transferID string) (Transfer, error)
	Release(ctx context.Context, transferID, reason string) (Transfer, error)
}
