// Package pgstore is the PostgreSQL implementation of ledger.Store.
//
// Concurrency model: every write runs in a READ COMMITTED transaction that
// locks the account rows it touches with SELECT ... FOR UPDATE, always in
// sorted id order so two transfers can't deadlock each other. Idempotency
// comes from two unique constraints: transfers.idempotency_key and
// entries (transfer_id, phase, direction).
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
)

type Store struct {
	pool *pgxpool.Pool
}

var _ ledger.Store = (*Store)(nil)

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Open connects and pings. Caller owns the returned pool.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

const transferCols = `id, idempotency_key, request_hash, kind, from_account,
	coalesce(to_account, ''), coalesce(destination, ''), amount, memo, status,
	failure_reason, tx_hash, created_at, updated_at`

func scanTransfer(row pgx.Row) (ledger.Transfer, error) {
	var t ledger.Transfer
	var amount int64
	err := row.Scan(&t.ID, &t.IdempotencyKey, &t.RequestHash, &t.Kind, &t.FromAccount,
		&t.ToAccount, &t.Destination, &amount, &t.Memo, &t.Status,
		&t.FailureReason, &t.TxHash, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ledger.ErrNotFound
	}
	t.Amount = money.FromMicros(amount)
	return t, err
}

func (s *Store) CreateAccount(ctx context.Context, na ledger.NewAccount) (ledger.Account, error) {
	id := na.ID
	if id == "" {
		id = ledger.NewID("acct")
	}
	a := ledger.Account{ID: id, Name: na.Name, AllowNegative: na.AllowNegative}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO accounts (id, name, allow_negative) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING
		RETURNING created_at`, id, na.Name, na.AllowNegative).Scan(&a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Account{}, ledger.ErrAccountExists
	}
	return a, err
}

func (s *Store) GetAccount(ctx context.Context, id string) (ledger.Account, error) {
	var a ledger.Account
	var bal int64
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, balance, allow_negative, created_at FROM accounts WHERE id = $1`, id).
		Scan(&a.ID, &a.Name, &bal, &a.AllowNegative, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ledger.ErrNotFound
	}
	a.Balance = money.FromMicros(bal)
	return a, err
}

func (s *Store) GetTransfer(ctx context.Context, id string) (ledger.Transfer, error) {
	return scanTransfer(s.pool.QueryRow(ctx, `SELECT `+transferCols+` FROM transfers WHERE id = $1`, id))
}

func (s *Store) ListEntries(ctx context.Context, accountID string, limit int) ([]ledger.Entry, error) {
	if _, err := s.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	} else if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, transfer_id, account_id, direction, amount, phase, created_at
		FROM entries WHERE account_id = $1 ORDER BY id DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ledger.Entry, error) {
		var e ledger.Entry
		var amt int64
		err := r.Scan(&e.ID, &e.TransferID, &e.AccountID, &e.Direction, &amt, &e.Phase, &e.CreatedAt)
		e.Amount = money.FromMicros(amt)
		return e, err
	})
}

func (s *Store) CreateTransfer(ctx context.Context, req ledger.TransferRequest) (ledger.Transfer, bool, error) {
	if err := req.Validate(); err != nil {
		return ledger.Transfer{}, false, err
	}
	hash := req.Hash()

	// Fast path: most retries hit a key that is already committed, so answer
	// those without taking any account locks.
	if t, err := s.byKey(ctx, s.pool, req.IdempotencyKey); err == nil {
		return replay(t, hash)
	} else if !errors.Is(err, ledger.ErrNotFound) {
		return ledger.Transfer{}, false, err
	}

	var out ledger.Transfer
	var replayed bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		credit := req.ToAccount
		if req.Kind == ledger.KindWithdrawal {
			credit = ledger.AccountSettlement
		}
		accts, err := lockAccounts(ctx, tx, req.FromAccount, credit)
		if err != nil {
			return err
		}
		from, ok := accts[req.FromAccount]
		if !ok {
			return fmt.Errorf("from account %q: %w", req.FromAccount, ledger.ErrNotFound)
		}
		if _, ok := accts[credit]; !ok {
			return fmt.Errorf("to account %q: %w", credit, ledger.ErrNotFound)
		}

		t := ledger.Transfer{
			ID:             ledger.NewID("tr"),
			IdempotencyKey: req.IdempotencyKey,
			RequestHash:    hash,
			Kind:           req.Kind,
			FromAccount:    req.FromAccount,
			ToAccount:      req.ToAccount,
			Destination:    req.Destination,
			Amount:         req.Amount,
			Memo:           req.Memo,
		}
		phase := ledger.PhasePost
		switch {
		case !from.allowNegative && from.balance < req.Amount.Micros():
			t.Status = ledger.StatusRejected
			t.FailureReason = ledger.FailureInsufficientFunds
		case req.Kind == ledger.KindInternal:
			t.Status = ledger.StatusPosted
		default:
			t.Status = ledger.StatusPending
			phase = ledger.PhaseHold
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO transfers (id, idempotency_key, request_hash, kind, from_account,
				to_account, destination, amount, memo, status, failure_reason)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), $8, $9, $10, $11)
			ON CONFLICT (idempotency_key) DO NOTHING
			RETURNING created_at, updated_at`,
			t.ID, t.IdempotencyKey, t.RequestHash, t.Kind, t.FromAccount,
			t.ToAccount, t.Destination, t.Amount.Micros(), t.Memo, t.Status, t.FailureReason,
		).Scan(&t.CreatedAt, &t.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			// Someone committed the same key while we waited on the locks.
			existing, err := s.byKey(ctx, tx, req.IdempotencyKey)
			if err != nil {
				return err
			}
			out, replayed, err = replay(existing, hash)
			return err
		}
		if err != nil {
			return err
		}
		if t.Status != ledger.StatusRejected {
			if err := post(ctx, tx, t.ID, phase, req.FromAccount, credit, t.Amount); err != nil {
				return err
			}
		}
		out = t
		return nil
	})
	return out, replayed, err
}

func replay(t ledger.Transfer, hash string) (ledger.Transfer, bool, error) {
	if t.RequestHash != hash {
		return ledger.Transfer{}, false, ledger.ErrIdempotencyConflict
	}
	return t, true, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) byKey(ctx context.Context, q querier, key string) (ledger.Transfer, error) {
	return scanTransfer(q.QueryRow(ctx, `SELECT `+transferCols+` FROM transfers WHERE idempotency_key = $1`, key))
}

type lockedAccount struct {
	balance       int64
	allowNegative bool
}

// lockAccounts takes row locks in id order. Everything that updates
// balances goes through here first, which is what keeps us deadlock free.
func lockAccounts(ctx context.Context, tx pgx.Tx, ids ...string) (map[string]lockedAccount, error) {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	rows, err := tx.Query(ctx, `
		SELECT id, balance, allow_negative FROM accounts
		WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]lockedAccount, len(ids))
	for rows.Next() {
		var id string
		var a lockedAccount
		if err := rows.Scan(&id, &a.balance, &a.allowNegative); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}

// post writes one debit/credit pair for (transferID, phase) and moves the
// balances. If the phase was already posted it does nothing. Callers must
// have locked both accounts.
func post(ctx context.Context, tx pgx.Tx, transferID string, phase ledger.Phase, debit, credit string, amt money.Amount) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO entries (transfer_id, account_id, direction, amount, phase)
		VALUES ($1, $2, 'debit', $4, $5), ($1, $3, 'credit', $4, $5)
		ON CONFLICT (transfer_id, phase, direction) DO NOTHING`,
		transferID, debit, credit, amt.Micros(), phase)
	if err != nil {
		return err
	}
	switch tag.RowsAffected() {
	case 0:
		return nil // already posted
	case 2:
	default:
		return fmt.Errorf("pgstore: half-posted entries for %s/%s", transferID, phase)
	}
	_, err = tx.Exec(ctx, `
		UPDATE accounts
		SET balance = balance + CASE WHEN id = $1 THEN -$3::bigint ELSE $3::bigint END
		WHERE id IN ($1, $2)`, debit, credit, amt.Micros())
	return err
}

// lockWithdrawal loads and row-locks a withdrawal.
func lockWithdrawal(ctx context.Context, tx pgx.Tx, id string) (ledger.Transfer, error) {
	t, err := scanTransfer(tx.QueryRow(ctx, `SELECT `+transferCols+` FROM transfers WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return t, err
	}
	if t.Kind != ledger.KindWithdrawal {
		return t, fmt.Errorf("%w: %s is not a withdrawal", ledger.ErrInvalidState, id)
	}
	return t, nil
}

func setStatus(ctx context.Context, tx pgx.Tx, t *ledger.Transfer) error {
	return tx.QueryRow(ctx, `
		UPDATE transfers SET status = $2, tx_hash = $3, failure_reason = $4, updated_at = now()
		WHERE id = $1 RETURNING updated_at`,
		t.ID, t.Status, t.TxHash, t.FailureReason).Scan(&t.UpdatedAt)
}

func (s *Store) MarkSubmitted(ctx context.Context, id, txHash string) (ledger.Transfer, error) {
	if txHash == "" {
		return ledger.Transfer{}, fmt.Errorf("%w: tx hash is required", ledger.ErrInvalidRequest)
	}
	var out ledger.Transfer
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		t, err := lockWithdrawal(ctx, tx, id)
		if err != nil {
			return err
		}
		switch t.Status {
		case ledger.StatusPending, ledger.StatusSubmitted:
			if t.Status == ledger.StatusSubmitted && t.TxHash == txHash {
				out = t
				return nil
			}
			t.Status = ledger.StatusSubmitted
			t.TxHash = txHash
			if err := setStatus(ctx, tx, &t); err != nil {
				return err
			}
		default:
			if t.TxHash != txHash {
				return fmt.Errorf("%w: transfer is %s", ledger.ErrInvalidState, t.Status)
			}
		}
		out = t
		return nil
	})
	return out, err
}

func (s *Store) Settle(ctx context.Context, id string) (ledger.Transfer, error) {
	var out ledger.Transfer
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		t, err := lockWithdrawal(ctx, tx, id)
		if err != nil {
			return err
		}
		switch t.Status {
		case ledger.StatusSettled:
			out = t
			return nil
		case ledger.StatusSubmitted:
		default:
			return fmt.Errorf("%w: cannot settle a %s transfer", ledger.ErrInvalidState, t.Status)
		}
		if _, err := lockAccounts(ctx, tx, ledger.AccountSettlement, ledger.AccountOnchainOut); err != nil {
			return err
		}
		if err := post(ctx, tx, t.ID, ledger.PhaseSettle, ledger.AccountSettlement, ledger.AccountOnchainOut, t.Amount); err != nil {
			return err
		}
		t.Status = ledger.StatusSettled
		if err := setStatus(ctx, tx, &t); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

func (s *Store) Release(ctx context.Context, id, reason string) (ledger.Transfer, error) {
	var out ledger.Transfer
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		t, err := lockWithdrawal(ctx, tx, id)
		if err != nil {
			return err
		}
		switch t.Status {
		case ledger.StatusFailed:
			out = t
			return nil
		case ledger.StatusPending, ledger.StatusSubmitted:
		default:
			return fmt.Errorf("%w: cannot release a %s transfer", ledger.ErrInvalidState, t.Status)
		}
		if _, err := lockAccounts(ctx, tx, ledger.AccountSettlement, t.FromAccount); err != nil {
			return err
		}
		if err := post(ctx, tx, t.ID, ledger.PhaseRelease, ledger.AccountSettlement, t.FromAccount, t.Amount); err != nil {
			return err
		}
		t.Status = ledger.StatusFailed
		t.FailureReason = reason
		if err := setStatus(ctx, tx, &t); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

// withTx runs fn in a transaction and retries a few times on deadlock or
// serialization errors. fn must be safe to re-run.
func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = pgx.BeginFunc(ctx, s.pool, fn)
		if !retryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 5 * time.Millisecond):
		}
	}
	return err
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", "40P01": // serialization_failure, deadlock_detected
			return true
		}
	}
	return false
}
