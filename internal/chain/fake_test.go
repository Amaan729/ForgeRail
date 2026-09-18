package chain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Amaan729/ForgeRail/internal/money"
)

var addr = "0x" + strings.Repeat("ab", 20)

func TestFakePrepareIsDeterministicPerRef(t *testing.T) {
	f := NewFake(FakeConfig{})
	ctx := context.Background()
	a, err := f.PrepareUSDCTransfer(ctx, "tr_1", addr, money.USDC(1))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.PrepareUSDCTransfer(ctx, "tr_1", addr, money.USDC(1))
	c, _ := f.PrepareUSDCTransfer(ctx, "tr_2", addr, money.USDC(1))
	if a.Hash != b.Hash {
		t.Error("same ref should give the same tx")
	}
	if a.Hash == c.Hash {
		t.Error("different refs should give different txs")
	}
	if _, err := f.PrepareUSDCTransfer(ctx, "tr_3", "0x12", 1); !errors.Is(err, ErrPermanent) {
		t.Errorf("bad address: want ErrPermanent, got %v", err)
	}
}

func TestFakeLifecycle(t *testing.T) {
	f := NewFake(FakeConfig{ConfirmAfter: 20 * time.Millisecond})
	ctx := context.Background()
	tx, _ := f.PrepareUSDCTransfer(ctx, "tr_1", addr, money.USDC(5))

	if r, _ := f.Receipt(ctx, tx.Hash); r.Status != TxNotFound {
		t.Fatalf("before broadcast: %s", r.Status)
	}
	for i := 0; i < 3; i++ {
		if err := f.Broadcast(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if f.Broadcasts() != 1 {
		t.Fatalf("rebroadcast should not create a second tx, got %d", f.Broadcasts())
	}
	if r, _ := f.Receipt(ctx, tx.Hash); r.Status != TxPending {
		t.Fatalf("right after broadcast: %s", r.Status)
	}
	time.Sleep(30 * time.Millisecond)
	if r, _ := f.Receipt(ctx, tx.Hash); r.Status != TxConfirmed {
		t.Fatalf("after ConfirmAfter: %s", r.Status)
	}
}

func TestFakeFailureInjection(t *testing.T) {
	f := NewFake(FakeConfig{BroadcastFailRate: 1, Seed: 1})
	ctx := context.Background()
	var failures int
	for i := 0; i < 50; i++ {
		tx, _ := f.PrepareUSDCTransfer(ctx, fmt.Sprintf("tr_%d", i), addr, 1)
		if err := f.Broadcast(ctx, tx); errors.Is(err, ErrFakeUnavailable) {
			failures++
		}
	}
	if failures != 50 {
		t.Fatalf("want every broadcast to fail, got %d/50", failures)
	}
	// but roughly half of them still landed on chain
	if n := f.Broadcasts(); n == 0 || n == 50 {
		t.Fatalf("expected some fail-after-accept cases, got %d accepted", n)
	}
}
