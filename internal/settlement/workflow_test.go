package settlement

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/Amaan729/ForgeRail/internal/chain"
	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
)

type fixture struct {
	env   *testsuite.TestWorkflowEnvironment
	acts  *Activities
	store *ledger.MemStore
	chain *chain.Fake
	user  string
	tr    ledger.Transfer
}

// newFixture funds a user with 100 USDC and opens a 25 USDC withdrawal.
func newFixture(t *testing.T, cfg chain.FakeConfig) *fixture {
	t.Helper()
	ctx := context.Background()
	store := ledger.NewMemStore()
	user, _ := store.CreateAccount(ctx, ledger.NewAccount{Name: "alice"})
	if _, _, err := store.CreateTransfer(ctx, ledger.TransferRequest{
		IdempotencyKey: "fund", Kind: ledger.KindInternal,
		FromAccount: ledger.AccountOnchainIn, ToAccount: user.ID, Amount: money.USDC(100),
	}); err != nil {
		t.Fatal(err)
	}
	tr, _, err := store.CreateTransfer(ctx, ledger.TransferRequest{
		IdempotencyKey: "wd", Kind: ledger.KindWithdrawal, FromAccount: user.ID,
		Destination: "0x" + strings.Repeat("c0", 20), Amount: money.USDC(25),
	})
	if err != nil || tr.Status != ledger.StatusPending {
		t.Fatalf("withdrawal: %+v %v", tr, err)
	}

	fake := chain.NewFake(cfg)
	acts := &Activities{Store: store, Chain: fake, PollInterval: time.Millisecond, MaxWait: 100 * time.Millisecond}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(acts)
	return &fixture{env: env, acts: acts, store: store, chain: fake, user: user.ID, tr: tr}
}

func (f *fixture) run(t *testing.T) (WithdrawalResult, error) {
	t.Helper()
	f.env.ExecuteWorkflow(WithdrawalWorkflow, WithdrawalInput{TransferID: f.tr.ID})
	if !f.env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	var res WithdrawalResult
	if err := f.env.GetWorkflowError(); err != nil {
		return res, err
	}
	if err := f.env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

func (f *fixture) balance(t *testing.T, id string) money.Amount {
	t.Helper()
	a, err := f.store.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Balance
}

func (f *fixture) transfer(t *testing.T) ledger.Transfer {
	t.Helper()
	tr, err := f.store.GetTransfer(context.Background(), f.tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestWithdrawalSettles(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{})
	res, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ledger.StatusSettled || res.TxHash == "" {
		t.Fatalf("unexpected result %+v", res)
	}
	tr := f.transfer(t)
	if tr.Status != ledger.StatusSettled || tr.TxHash != res.TxHash {
		t.Fatalf("stored transfer %+v", tr)
	}
	if got := f.balance(t, f.user); got != money.USDC(75) {
		t.Errorf("user balance %s", got)
	}
	if got := f.balance(t, ledger.AccountSettlement); got != 0 {
		t.Errorf("settlement balance %s", got)
	}
	if got := f.balance(t, ledger.AccountOnchainOut); got != money.USDC(25) {
		t.Errorf("onchain_out balance %s", got)
	}
	if f.chain.Broadcasts() != 1 {
		t.Errorf("broadcasts = %d", f.chain.Broadcasts())
	}
}

func TestRevertedTxReleasesFunds(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{RevertRate: 1})
	res, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ledger.StatusFailed || !strings.Contains(res.FailureReason, "reverted") {
		t.Fatalf("unexpected result %+v", res)
	}
	if got := f.balance(t, f.user); got != money.USDC(100) {
		t.Errorf("user should be made whole, balance %s", got)
	}
	if got := f.balance(t, ledger.AccountSettlement); got != 0 {
		t.Errorf("settlement balance %s", got)
	}
}

func TestFlakyBroadcastStillSettlesOnce(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{BroadcastFailRate: 0.6, Seed: 42})
	res, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ledger.StatusSettled {
		t.Fatalf("unexpected result %+v", res)
	}
	if f.chain.Broadcasts() != 1 {
		t.Errorf("tx reached the chain %d times", f.chain.Broadcasts())
	}
	if got := f.balance(t, f.user); got != money.USDC(75) {
		t.Errorf("user balance %s", got)
	}
}

func TestTxThatNeverLandsIsReleased(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{})
	f.env.OnActivity(f.acts.Broadcast, mock.Anything, mock.Anything).
		Return(temporal.NewNonRetryableApplicationError("node rejected tx", ErrTypeChainPermanent, nil))

	res, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ledger.StatusFailed || res.FailureReason != "transaction never reached the chain" {
		t.Fatalf("unexpected result %+v", res)
	}
	tr := f.transfer(t)
	if tr.Status != ledger.StatusFailed || tr.TxHash == "" {
		t.Fatalf("stored transfer %+v", tr)
	}
	if got := f.balance(t, f.user); got != money.USDC(100) {
		t.Errorf("user balance %s", got)
	}
}

func TestPrepareFailureReleasesWithoutSubmitting(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{})
	f.env.OnActivity(f.acts.PrepareTx, mock.Anything, mock.Anything).
		Return(chain.SignedTx{}, temporal.NewNonRetryableApplicationError("hot wallet empty", ErrTypeChainPermanent, nil))

	res, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ledger.StatusFailed || !strings.Contains(res.FailureReason, "hot wallet empty") {
		t.Fatalf("unexpected result %+v", res)
	}
	if tr := f.transfer(t); tr.TxHash != "" {
		t.Errorf("nothing should have been submitted, got hash %s", tr.TxHash)
	}
	if got := f.balance(t, f.user); got != money.USDC(100) {
		t.Errorf("user balance %s", got)
	}
}

func TestUnknownReceiptStateFailsWorkflowWithoutReleasing(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{})
	f.env.OnActivity(f.acts.WaitForReceipt, mock.Anything, mock.Anything).
		Return(chain.Receipt{}, temporal.NewApplicationError("rpc timeout", "RPC"))

	if _, err := f.run(t); err == nil {
		t.Fatal("expected workflow error")
	}
	// we don't know if the tx made it, so the hold must stay in place
	if tr := f.transfer(t); tr.Status != ledger.StatusSubmitted {
		t.Fatalf("status = %s, want submitted", tr.Status)
	}
	if got := f.balance(t, ledger.AccountSettlement); got != money.USDC(25) {
		t.Errorf("settlement balance %s", got)
	}
}

func TestRerunOnSettledTransferIsNoop(t *testing.T) {
	f := newFixture(t, chain.FakeConfig{})
	if _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	entriesBefore, _ := f.store.ListEntries(context.Background(), f.user, 100)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(f.acts)
	env.ExecuteWorkflow(WithdrawalWorkflow, WithdrawalInput{TransferID: f.tr.ID})
	var res WithdrawalResult
	if err := env.GetWorkflowResult(&res); err != nil || res.Status != ledger.StatusSettled {
		t.Fatalf("rerun: %+v %v", res, err)
	}
	entriesAfter, _ := f.store.ListEntries(context.Background(), f.user, 100)
	if len(entriesAfter) != len(entriesBefore) {
		t.Errorf("rerun posted new entries: %d -> %d", len(entriesBefore), len(entriesAfter))
	}
	if f.chain.Broadcasts() != 1 {
		t.Errorf("broadcasts = %d", f.chain.Broadcasts())
	}
}
