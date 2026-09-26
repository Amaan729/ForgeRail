package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
)

type recordingStarter struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recordingStarter) StartWithdrawal(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, id)
	return r.err
}

func setup(t *testing.T, allowSys bool) (*Service, *recordingStarter, string) {
	t.Helper()
	store := ledger.NewMemStore()
	starter := &recordingStarter{}
	svc := &Service{Store: store, Starter: starter, AllowSystemTransfers: allowSys}
	ctx := context.Background()
	a, _ := store.CreateAccount(ctx, ledger.NewAccount{Name: "a"})
	store.CreateTransfer(ctx, ledger.TransferRequest{
		IdempotencyKey: "fund", Kind: ledger.KindInternal,
		FromAccount: ledger.AccountOnchainIn, ToAccount: a.ID, Amount: money.USDC(10),
	})
	return svc, starter, a.ID
}

func withdrawal(from string) ledger.TransferRequest {
	return ledger.TransferRequest{
		IdempotencyKey: "w", Kind: ledger.KindWithdrawal, FromAccount: from,
		Destination: "0x" + strings.Repeat("9", 40), Amount: money.USDC(1),
	}
}

func TestSystemTransfersBlockedByDefault(t *testing.T) {
	svc, _, a := setup(t, false)
	_, _, err := svc.CreateTransfer(context.Background(), ledger.TransferRequest{
		IdempotencyKey: "mint", Kind: ledger.KindInternal,
		FromAccount: ledger.AccountOnchainIn, ToAccount: a, Amount: money.USDC(1_000_000),
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestWithdrawalStartsSettlementOnEveryRetry(t *testing.T) {
	svc, starter, a := setup(t, false)
	ctx := context.Background()
	first, _, err := svc.CreateTransfer(ctx, withdrawal(a))
	if err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := svc.CreateTransfer(ctx, withdrawal(a)); err != nil || !replayed {
		t.Fatalf("retry: replayed=%v err=%v", replayed, err)
	}
	if len(starter.calls) != 2 || starter.calls[0] != first.ID || starter.calls[1] != first.ID {
		t.Fatalf("starter calls = %v", starter.calls)
	}
}

func TestStarterFailureIsReportedButTransferKept(t *testing.T) {
	svc, starter, a := setup(t, false)
	starter.err = errors.New("temporal down")
	tr, _, err := svc.CreateTransfer(context.Background(), withdrawal(a))
	if !errors.Is(err, ErrSettlementUnavailable) {
		t.Fatalf("want ErrSettlementUnavailable, got %v", err)
	}
	if tr.ID == "" || tr.Status != ledger.StatusPending {
		t.Fatalf("transfer should still come back: %+v", tr)
	}
	// once the starter recovers the same key goes through
	starter.err = nil
	again, replayed, err := svc.CreateTransfer(context.Background(), withdrawal(a))
	if err != nil || !replayed || again.ID != tr.ID {
		t.Fatalf("retry after recovery: %+v replayed=%v err=%v", again, replayed, err)
	}
}

func TestInternalTransferDoesNotStartSettlement(t *testing.T) {
	svc, starter, a := setup(t, false)
	ctx := context.Background()
	b, _ := svc.CreateAccount(ctx, "b")
	if _, _, err := svc.CreateTransfer(ctx, ledger.TransferRequest{
		IdempotencyKey: "i", Kind: ledger.KindInternal, FromAccount: a, ToAccount: b.ID, Amount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if len(starter.calls) != 0 {
		t.Fatalf("starter called for internal transfer: %v", starter.calls)
	}
}
