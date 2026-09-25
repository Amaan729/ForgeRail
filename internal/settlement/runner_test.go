package settlement

import (
	"context"
	"sync"
	"testing"

	"github.com/Amaan729/ForgeRail/internal/chain"
	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
)

func TestLocalRunnerSettlesWithFlakyChain(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{BroadcastFailRate: 0.5, Seed: 3})
	r := &LocalRunner{Acts: f.acts, Backoff: 1}
	res, err := r.Run(context.Background(), f.tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ledger.StatusSettled {
		t.Fatalf("unexpected result %+v", res)
	}
	if f.chain.Broadcasts() != 1 {
		t.Errorf("broadcasts = %d", f.chain.Broadcasts())
	}
	if got := f.balance(t, ledger.AccountOnchainOut); got != money.USDC(25) {
		t.Errorf("onchain_out balance %s", got)
	}
}

func TestLocalRunnerReleasesOnRevert(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{RevertRate: 1})
	r := &LocalRunner{Acts: f.acts, Backoff: 1}
	res, err := r.Run(context.Background(), f.tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ledger.StatusFailed {
		t.Fatalf("unexpected result %+v", res)
	}
	if got := f.balance(t, f.user); got != money.USDC(100) {
		t.Errorf("user balance %s", got)
	}
}

func TestLocalRunnerIgnoresDuplicateStarts(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{})
	r := &LocalRunner{Acts: f.acts, Backoff: 1}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.StartWithdrawal(context.Background(), f.tr.ID)
		}()
	}
	wg.Wait()
	r.Wait()
	// and once more after it finished: should see "settled" and do nothing
	_ = r.StartWithdrawal(context.Background(), f.tr.ID)
	r.Wait()

	if tr := f.transfer(t); tr.Status != ledger.StatusSettled {
		t.Fatalf("status = %s", tr.Status)
	}
	if f.chain.Broadcasts() != 1 {
		t.Errorf("broadcasts = %d", f.chain.Broadcasts())
	}
	if got := f.balance(t, ledger.AccountOnchainOut); got != money.USDC(25) {
		t.Errorf("onchain_out balance %s (settled more than once?)", got)
	}
}
