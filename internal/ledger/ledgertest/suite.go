// Package ledgertest is a conformance suite that every ledger.Store must pass.
// The memory store runs it in plain `go test`; the Postgres store runs it when
// FORGERAIL_TEST_DATABASE_URL is set.
package ledgertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
)

// Factory returns an empty store (system accounts only) for one test.
type Factory func(t *testing.T) ledger.Store

var testAddr = "0x" + strings.Repeat("1f", 20)

func Run(t *testing.T, newStore Factory) {
	tests := []struct {
		name string
		fn   func(*testing.T, ledger.Store)
	}{
		{"Accounts", testAccounts},
		{"InternalTransfer", testInternalTransfer},
		{"ReplaySameKey", testReplaySameKey},
		{"KeyReuseDifferentBody", testKeyReuseDifferentBody},
		{"InsufficientFunds", testInsufficientFunds},
		{"UnknownAccount", testUnknownAccount},
		{"WithdrawalSettles", testWithdrawalSettles},
		{"WithdrawalReleases", testWithdrawalReleases},
		{"ConcurrentDuplicates", testConcurrentDuplicates},
		{"NoOverdraftUnderConcurrency", testNoOverdraft},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, newStore(t)) })
	}
}

var ctx = context.Background()

func mustAccount(t *testing.T, s ledger.Store, name string) ledger.Account {
	t.Helper()
	a, err := s.CreateAccount(ctx, ledger.NewAccount{Name: name})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return a
}

// deposit funds an account from the on-chain inflow account.
func deposit(t *testing.T, s ledger.Store, acct string, amt money.Amount) {
	t.Helper()
	tr, _, err := s.CreateTransfer(ctx, ledger.TransferRequest{
		IdempotencyKey: "deposit-" + ledger.NewID("k"),
		Kind:           ledger.KindInternal,
		FromAccount:    ledger.AccountOnchainIn,
		ToAccount:      acct,
		Amount:         amt,
	})
	if err != nil || tr.Status != ledger.StatusPosted {
		t.Fatalf("deposit: status=%s err=%v", tr.Status, err)
	}
}

func balance(t *testing.T, s ledger.Store, id string) money.Amount {
	t.Helper()
	a, err := s.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("get account %s: %v", id, err)
	}
	return a.Balance
}

func wantBalance(t *testing.T, s ledger.Store, id string, want money.Amount) {
	t.Helper()
	if got := balance(t, s, id); got != want {
		t.Errorf("balance(%s) = %s, want %s", id, got, want)
	}
}

// checkConservation asserts that the given accounts (plus system accounts)
// sum to zero. Valid when the store started empty.
func checkConservation(t *testing.T, s ledger.Store, ids ...string) {
	t.Helper()
	ids = append(ids, ledger.AccountOnchainIn, ledger.AccountSettlement, ledger.AccountOnchainOut)
	var sum money.Amount
	for _, id := range ids {
		sum += balance(t, s, id)
	}
	if sum != 0 {
		t.Errorf("ledger does not balance: sum = %s", sum)
	}
}

func testAccounts(t *testing.T, s ledger.Store) {
	a := mustAccount(t, s, "alice")
	if !strings.HasPrefix(a.ID, "acct_") || a.Balance != 0 {
		t.Fatalf("unexpected account %+v", a)
	}
	got, err := s.GetAccount(ctx, a.ID)
	if err != nil || got.Name != "alice" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.CreateAccount(ctx, ledger.NewAccount{ID: a.ID}); !errors.Is(err, ledger.ErrAccountExists) {
		t.Errorf("duplicate id: want ErrAccountExists, got %v", err)
	}
	if _, err := s.GetAccount(ctx, "acct_missing"); !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("missing account: want ErrNotFound, got %v", err)
	}
	named, err := s.CreateAccount(ctx, ledger.NewAccount{ID: "acct_fixed", Name: "fixed"})
	if err != nil || named.ID != "acct_fixed" {
		t.Errorf("explicit id: %+v %v", named, err)
	}
}

func testInternalTransfer(t *testing.T, s ledger.Store) {
	a, b := mustAccount(t, s, "a"), mustAccount(t, s, "b")
	deposit(t, s, a.ID, money.USDC(100))

	tr, replayed, err := s.CreateTransfer(ctx, ledger.TransferRequest{
		IdempotencyKey: "t1", Kind: ledger.KindInternal,
		FromAccount: a.ID, ToAccount: b.ID, Amount: money.MustParse("40.25"), Memo: "rent",
	})
	if err != nil || replayed {
		t.Fatalf("create: replayed=%v err=%v", replayed, err)
	}
	if tr.Status != ledger.StatusPosted || tr.Memo != "rent" {
		t.Fatalf("unexpected transfer %+v", tr)
	}
	wantBalance(t, s, a.ID, money.MustParse("59.75"))
	wantBalance(t, s, b.ID, money.MustParse("40.25"))

	got, err := s.GetTransfer(ctx, tr.ID)
	if err != nil || got.IdempotencyKey != "t1" || got.Amount != tr.Amount {
		t.Fatalf("get transfer: %+v %v", got, err)
	}

	entries, err := s.ListEntries(ctx, b.ID, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries for b: %v %v", entries, err)
	}
	e := entries[0]
	if e.Direction != ledger.Credit || e.Phase != ledger.PhasePost || e.TransferID != tr.ID {
		t.Errorf("unexpected entry %+v", e)
	}
	checkConservation(t, s, a.ID, b.ID)
}

func testReplaySameKey(t *testing.T, s ledger.Store) {
	a, b := mustAccount(t, s, "a"), mustAccount(t, s, "b")
	deposit(t, s, a.ID, money.USDC(10))
	req := ledger.TransferRequest{IdempotencyKey: "same", Kind: ledger.KindInternal, FromAccount: a.ID, ToAccount: b.ID, Amount: money.USDC(3)}

	first, _, err := s.CreateTransfer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, replayed, err := s.CreateTransfer(ctx, req)
		if err != nil || !replayed || again.ID != first.ID {
			t.Fatalf("replay %d: id=%s replayed=%v err=%v", i, again.ID, replayed, err)
		}
	}
	wantBalance(t, s, a.ID, money.USDC(7))
	wantBalance(t, s, b.ID, money.USDC(3))
	entries, _ := s.ListEntries(ctx, a.ID, 100)
	if len(entries) != 2 { // deposit credit + one debit
		t.Errorf("want 2 entries on a, got %d", len(entries))
	}
}

func testKeyReuseDifferentBody(t *testing.T, s ledger.Store) {
	a, b := mustAccount(t, s, "a"), mustAccount(t, s, "b")
	deposit(t, s, a.ID, money.USDC(10))
	req := ledger.TransferRequest{IdempotencyKey: "k", Kind: ledger.KindInternal, FromAccount: a.ID, ToAccount: b.ID, Amount: money.USDC(1)}
	if _, _, err := s.CreateTransfer(ctx, req); err != nil {
		t.Fatal(err)
	}
	req.Amount = money.USDC(2)
	if _, _, err := s.CreateTransfer(ctx, req); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}
	wantBalance(t, s, a.ID, money.USDC(9))
}

func testInsufficientFunds(t *testing.T, s ledger.Store) {
	a, b := mustAccount(t, s, "a"), mustAccount(t, s, "b")
	deposit(t, s, a.ID, money.USDC(5))
	req := ledger.TransferRequest{IdempotencyKey: "big", Kind: ledger.KindInternal, FromAccount: a.ID, ToAccount: b.ID, Amount: money.MustParse("5.000001")}
	tr, _, err := s.CreateTransfer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != ledger.StatusRejected || tr.FailureReason != ledger.FailureInsufficientFunds {
		t.Fatalf("want rejected/insufficient_funds, got %s/%s", tr.Status, tr.FailureReason)
	}
	wantBalance(t, s, a.ID, money.USDC(5))
	wantBalance(t, s, b.ID, 0)

	// Retrying the same key keeps returning the rejection even after funding.
	deposit(t, s, a.ID, money.USDC(5))
	again, replayed, err := s.CreateTransfer(ctx, req)
	if err != nil || !replayed || again.Status != ledger.StatusRejected {
		t.Fatalf("replay of rejected transfer: %+v replayed=%v err=%v", again, replayed, err)
	}
	wantBalance(t, s, b.ID, 0)
}

func testUnknownAccount(t *testing.T, s ledger.Store) {
	a := mustAccount(t, s, "a")
	deposit(t, s, a.ID, money.USDC(1))
	_, _, err := s.CreateTransfer(ctx, ledger.TransferRequest{IdempotencyKey: "x", Kind: ledger.KindInternal, FromAccount: a.ID, ToAccount: "acct_nope", Amount: 1})
	if !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	_, _, err = s.CreateTransfer(ctx, ledger.TransferRequest{IdempotencyKey: "y", Kind: ledger.KindInternal, FromAccount: "acct_nope", ToAccount: a.ID, Amount: 1})
	if !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// A failed lookup must not burn the idempotency key.
	b := mustAccount(t, s, "b")
	tr, replayed, err := s.CreateTransfer(ctx, ledger.TransferRequest{IdempotencyKey: "x", Kind: ledger.KindInternal, FromAccount: a.ID, ToAccount: b.ID, Amount: 1})
	if err != nil || replayed || tr.Status != ledger.StatusPosted {
		t.Fatalf("key should still be usable: %+v replayed=%v err=%v", tr, replayed, err)
	}
}

func newWithdrawal(t *testing.T, s ledger.Store, from string, amt money.Amount, key string) ledger.Transfer {
	t.Helper()
	tr, _, err := s.CreateTransfer(ctx, ledger.TransferRequest{
		IdempotencyKey: key, Kind: ledger.KindWithdrawal, FromAccount: from, Destination: testAddr, Amount: amt,
	})
	if err != nil {
		t.Fatalf("create withdrawal: %v", err)
	}
	return tr
}

func testWithdrawalSettles(t *testing.T, s ledger.Store) {
	a := mustAccount(t, s, "a")
	deposit(t, s, a.ID, money.USDC(50))
	tr := newWithdrawal(t, s, a.ID, money.USDC(20), "w1")
	if tr.Status != ledger.StatusPending || tr.Destination != testAddr {
		t.Fatalf("want pending, got %+v", tr)
	}
	wantBalance(t, s, a.ID, money.USDC(30))
	wantBalance(t, s, ledger.AccountSettlement, money.USDC(20))

	if _, err := s.Settle(ctx, tr.ID); !errors.Is(err, ledger.ErrInvalidState) {
		t.Errorf("settle before submit: want ErrInvalidState, got %v", err)
	}
	for i := 0; i < 2; i++ {
		sub, err := s.MarkSubmitted(ctx, tr.ID, "0xabc")
		if err != nil || sub.Status != ledger.StatusSubmitted || sub.TxHash != "0xabc" {
			t.Fatalf("mark submitted: %+v %v", sub, err)
		}
	}
	for i := 0; i < 3; i++ {
		st, err := s.Settle(ctx, tr.ID)
		if err != nil || st.Status != ledger.StatusSettled {
			t.Fatalf("settle #%d: %+v %v", i, st, err)
		}
	}
	wantBalance(t, s, a.ID, money.USDC(30))
	wantBalance(t, s, ledger.AccountSettlement, 0)
	wantBalance(t, s, ledger.AccountOnchainOut, money.USDC(20))

	if _, err := s.Release(ctx, tr.ID, "late"); !errors.Is(err, ledger.ErrInvalidState) {
		t.Errorf("release after settle: want ErrInvalidState, got %v", err)
	}
	if _, err := s.MarkSubmitted(ctx, tr.ID, "0xabc"); err != nil {
		t.Errorf("late duplicate MarkSubmitted should be a no-op, got %v", err)
	}
	checkConservation(t, s, a.ID)
}

func testWithdrawalReleases(t *testing.T, s ledger.Store) {
	a := mustAccount(t, s, "a")
	deposit(t, s, a.ID, money.USDC(10))
	tr := newWithdrawal(t, s, a.ID, money.USDC(10), "w2")
	wantBalance(t, s, a.ID, 0)

	for i := 0; i < 3; i++ {
		rel, err := s.Release(ctx, tr.ID, "tx reverted")
		if err != nil || rel.Status != ledger.StatusFailed || rel.FailureReason != "tx reverted" {
			t.Fatalf("release #%d: %+v %v", i, rel, err)
		}
	}
	wantBalance(t, s, a.ID, money.USDC(10))
	wantBalance(t, s, ledger.AccountSettlement, 0)
	if _, err := s.Settle(ctx, tr.ID); !errors.Is(err, ledger.ErrInvalidState) {
		t.Errorf("settle after release: want ErrInvalidState, got %v", err)
	}

	internal, _, _ := s.CreateTransfer(ctx, ledger.TransferRequest{IdempotencyKey: "i", Kind: ledger.KindInternal, FromAccount: a.ID, ToAccount: ledger.AccountOnchainOut, Amount: 1})
	if _, err := s.Settle(ctx, internal.ID); !errors.Is(err, ledger.ErrInvalidState) {
		t.Errorf("settle on internal transfer: want ErrInvalidState, got %v", err)
	}
	checkConservation(t, s, a.ID)
}

func testConcurrentDuplicates(t *testing.T, s ledger.Store) {
	a, b := mustAccount(t, s, "a"), mustAccount(t, s, "b")
	deposit(t, s, a.ID, money.USDC(100))
	req := ledger.TransferRequest{IdempotencyKey: "dup", Kind: ledger.KindInternal, FromAccount: a.ID, ToAccount: b.ID, Amount: money.USDC(7)}

	const n = 25
	var fresh atomic.Int32
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr, replayed, err := s.CreateTransfer(ctx, req)
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			if !replayed {
				fresh.Add(1)
			}
			ids[i] = tr.ID
		}(i)
	}
	wg.Wait()
	if fresh.Load() != 1 {
		t.Errorf("want exactly one non-replayed response, got %d", fresh.Load())
	}
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("different transfer ids returned: %s vs %s", id, ids[0])
		}
	}
	wantBalance(t, s, a.ID, money.USDC(93))
	wantBalance(t, s, b.ID, money.USDC(7))
}

func testNoOverdraft(t *testing.T, s ledger.Store) {
	a, b := mustAccount(t, s, "a"), mustAccount(t, s, "b")
	deposit(t, s, a.ID, money.USDC(100))

	const n = 40
	var posted, rejected atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr, _, err := s.CreateTransfer(ctx, ledger.TransferRequest{
				IdempotencyKey: fmt.Sprintf("od-%d", i), Kind: ledger.KindInternal,
				FromAccount: a.ID, ToAccount: b.ID, Amount: money.USDC(10),
			})
			if err != nil {
				t.Errorf("transfer %d: %v", i, err)
				return
			}
			switch tr.Status {
			case ledger.StatusPosted:
				posted.Add(1)
			case ledger.StatusRejected:
				rejected.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if posted.Load() != 10 || rejected.Load() != n-10 {
		t.Errorf("posted=%d rejected=%d, want 10/%d", posted.Load(), rejected.Load(), n-10)
	}
	wantBalance(t, s, a.ID, 0)
	wantBalance(t, s, b.ID, money.USDC(100))
	checkConservation(t, s, a.ID, b.ID)
}
