package ledger

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Amaan729/ForgeRail/internal/money"
)

// MemStore is an in-memory Store. It is used by unit tests and by
// `forgerail -dev` when no database is configured. One mutex guards
// everything, which is plenty for tests and nowhere near fast enough for
// real traffic.
type MemStore struct {
	mu        sync.Mutex
	accounts  map[string]*Account
	transfers map[string]*Transfer
	byKey     map[string]string
	posted    map[string]bool // transferID + "/" + phase
	entries   []Entry
	nextEntry int64
}

var _ Store = (*MemStore)(nil)

func NewMemStore() *MemStore {
	s := &MemStore{
		accounts:  map[string]*Account{},
		transfers: map[string]*Transfer{},
		byKey:     map[string]string{},
		posted:    map[string]bool{},
	}
	now := time.Now().UTC()
	for _, a := range []Account{
		{ID: AccountOnchainIn, Name: "on-chain deposits", AllowNegative: true},
		{ID: AccountSettlement, Name: "withdrawals in flight"},
		{ID: AccountOnchainOut, Name: "on-chain withdrawals"},
	} {
		a.CreatedAt = now
		s.accounts[a.ID] = &a
	}
	return s
}

func (s *MemStore) CreateAccount(_ context.Context, na NewAccount) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := na.ID
	if id == "" {
		id = NewID("acct")
	}
	if _, ok := s.accounts[id]; ok {
		return Account{}, ErrAccountExists
	}
	a := &Account{ID: id, Name: na.Name, AllowNegative: na.AllowNegative, CreatedAt: time.Now().UTC()}
	s.accounts[id] = a
	return *a, nil
}

func (s *MemStore) GetAccount(_ context.Context, id string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return Account{}, ErrNotFound
	}
	return *a, nil
}

func (s *MemStore) CreateTransfer(_ context.Context, req TransferRequest) (Transfer, bool, error) {
	if err := req.Validate(); err != nil {
		return Transfer{}, false, err
	}
	hash := req.Hash()

	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.byKey[req.IdempotencyKey]; ok {
		t := s.transfers[id]
		if t.RequestHash != hash {
			return Transfer{}, false, ErrIdempotencyConflict
		}
		return *t, true, nil
	}

	from, ok := s.accounts[req.FromAccount]
	if !ok {
		return Transfer{}, false, fmt.Errorf("from account %q: %w", req.FromAccount, ErrNotFound)
	}
	var credit string
	switch req.Kind {
	case KindInternal:
		if _, ok := s.accounts[req.ToAccount]; !ok {
			return Transfer{}, false, fmt.Errorf("to account %q: %w", req.ToAccount, ErrNotFound)
		}
		credit = req.ToAccount
	case KindWithdrawal:
		credit = AccountSettlement
	}

	now := time.Now().UTC()
	t := &Transfer{
		ID:             NewID("tr"),
		IdempotencyKey: req.IdempotencyKey,
		RequestHash:    hash,
		Kind:           req.Kind,
		FromAccount:    req.FromAccount,
		ToAccount:      req.ToAccount,
		Destination:    req.Destination,
		Amount:         req.Amount,
		Memo:           req.Memo,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	switch {
	case !from.AllowNegative && from.Balance < req.Amount:
		t.Status = StatusRejected
		t.FailureReason = FailureInsufficientFunds
	case req.Kind == KindInternal:
		s.post(t.ID, PhasePost, from.ID, credit, req.Amount, now)
		t.Status = StatusPosted
	default:
		s.post(t.ID, PhaseHold, from.ID, credit, req.Amount, now)
		t.Status = StatusPending
	}

	s.transfers[t.ID] = t
	s.byKey[t.IdempotencyKey] = t.ID
	return *t, false, nil
}

// post writes one balanced debit/credit pair. Caller holds s.mu.
func (s *MemStore) post(transferID string, phase Phase, debit, credit string, amt money.Amount, now time.Time) {
	key := transferID + "/" + string(phase)
	if s.posted[key] {
		return
	}
	s.posted[key] = true
	s.nextEntry++
	s.entries = append(s.entries, Entry{ID: s.nextEntry, TransferID: transferID, AccountID: debit, Direction: Debit, Amount: amt, Phase: phase, CreatedAt: now})
	s.nextEntry++
	s.entries = append(s.entries, Entry{ID: s.nextEntry, TransferID: transferID, AccountID: credit, Direction: Credit, Amount: amt, Phase: phase, CreatedAt: now})
	s.accounts[debit].Balance -= amt
	s.accounts[credit].Balance += amt
}

func (s *MemStore) GetTransfer(_ context.Context, id string) (Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[id]
	if !ok {
		return Transfer{}, ErrNotFound
	}
	return *t, nil
}

func (s *MemStore) ListEntries(_ context.Context, accountID string, limit int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[accountID]; !ok {
		return nil, ErrNotFound
	}
	limit = clampLimit(limit)
	var out []Entry
	for i := len(s.entries) - 1; i >= 0 && len(out) < limit; i-- {
		if s.entries[i].AccountID == accountID {
			out = append(out, s.entries[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (s *MemStore) withdrawal(id string) (*Transfer, error) {
	t, ok := s.transfers[id]
	if !ok {
		return nil, ErrNotFound
	}
	if t.Kind != KindWithdrawal {
		return nil, fmt.Errorf("%w: %s is not a withdrawal", ErrInvalidState, id)
	}
	return t, nil
}

func (s *MemStore) MarkSubmitted(_ context.Context, id, txHash string) (Transfer, error) {
	if txHash == "" {
		return Transfer{}, fmt.Errorf("%w: tx hash is required", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.withdrawal(id)
	if err != nil {
		return Transfer{}, err
	}
	switch t.Status {
	case StatusPending, StatusSubmitted:
		t.Status = StatusSubmitted
		t.TxHash = txHash
		t.UpdatedAt = time.Now().UTC()
	default:
		if t.TxHash != txHash {
			return Transfer{}, fmt.Errorf("%w: transfer is %s", ErrInvalidState, t.Status)
		}
	}
	return *t, nil
}

func (s *MemStore) Settle(_ context.Context, id string) (Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.withdrawal(id)
	if err != nil {
		return Transfer{}, err
	}
	switch t.Status {
	case StatusSettled:
		return *t, nil
	case StatusSubmitted:
		now := time.Now().UTC()
		s.post(t.ID, PhaseSettle, AccountSettlement, AccountOnchainOut, t.Amount, now)
		t.Status = StatusSettled
		t.UpdatedAt = now
		return *t, nil
	}
	return Transfer{}, fmt.Errorf("%w: cannot settle a %s transfer", ErrInvalidState, t.Status)
}

func (s *MemStore) Release(_ context.Context, id, reason string) (Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.withdrawal(id)
	if err != nil {
		return Transfer{}, err
	}
	switch t.Status {
	case StatusFailed:
		return *t, nil
	case StatusPending, StatusSubmitted:
		now := time.Now().UTC()
		s.post(t.ID, PhaseRelease, AccountSettlement, t.FromAccount, t.Amount, now)
		t.Status = StatusFailed
		t.FailureReason = reason
		t.UpdatedAt = now
		return *t, nil
	}
	return Transfer{}, fmt.Errorf("%w: cannot release a %s transfer", ErrInvalidState, t.Status)
}

func clampLimit(n int) int {
	if n <= 0 {
		return 100
	}
	if n > 1000 {
		return 1000
	}
	return n
}
