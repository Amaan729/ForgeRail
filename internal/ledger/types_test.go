package ledger

import (
	"errors"
	"strings"
	"testing"

	"github.com/Amaan729/ForgeRail/internal/money"
)

func TestValidate(t *testing.T) {
	good := TransferRequest{
		IdempotencyKey: "k1",
		Kind:           KindInternal,
		FromAccount:    "acct_a",
		ToAccount:      "acct_b",
		Amount:         money.USDC(5),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	wd := TransferRequest{
		IdempotencyKey: "k2",
		Kind:           KindWithdrawal,
		FromAccount:    "acct_a",
		Destination:    "0x" + strings.Repeat("ab", 20),
		Amount:         money.USDC(1),
	}
	if err := wd.Validate(); err != nil {
		t.Fatalf("valid withdrawal rejected: %v", err)
	}

	bad := []func(r *TransferRequest){
		func(r *TransferRequest) { r.IdempotencyKey = " " },
		func(r *TransferRequest) { r.Amount = 0 },
		func(r *TransferRequest) { r.Amount = -1 },
		func(r *TransferRequest) { r.ToAccount = r.FromAccount },
		func(r *TransferRequest) { r.ToAccount = "" },
		func(r *TransferRequest) { r.Kind = "swap" },
		func(r *TransferRequest) { r.Destination = "0x1234" },
	}
	for i, mutate := range bad {
		r := good
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("case %d: want ErrInvalidRequest, got %v", i, err)
		}
	}

	badWd := wd
	badWd.Destination = "0xnothex" + strings.Repeat("0", 33)
	if err := badWd.Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("bad address accepted: %v", err)
	}
}

func TestHashIgnoresKeyButNotBody(t *testing.T) {
	a := TransferRequest{IdempotencyKey: "a", Kind: KindInternal, FromAccount: "x", ToAccount: "y", Amount: 10}
	b := a
	b.IdempotencyKey = "b"
	if a.Hash() != b.Hash() {
		t.Error("hash should not depend on the idempotency key")
	}
	c := a
	c.Amount = 11
	if a.Hash() == c.Hash() {
		t.Error("hash should change when the amount changes")
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID("tr")
		if !strings.HasPrefix(id, "tr_") || len(id) != 3+28 {
			t.Fatalf("unexpected id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
