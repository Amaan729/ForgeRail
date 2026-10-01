package settlement_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"github.com/Amaan729/ForgeRail/internal/chain"
	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
	"github.com/Amaan729/ForgeRail/internal/service"
	"github.com/Amaan729/ForgeRail/internal/settlement"
)

// These run against a real Temporal server, e.g. `temporal server start-dev`:
//
//	FORGERAIL_TEST_TEMPORAL_ADDR=localhost:7233 go test ./internal/settlement/ -run Temporal
//
// Each test gets its own task queue so they don't steal each other's work.

type temporalEnv struct {
	client  client.Client
	starter settlement.TemporalStarter
	store   *ledger.MemStore
	chain   *chain.Fake
	user    string
}

func newTemporalEnv(t *testing.T, cfg chain.FakeConfig) *temporalEnv {
	t.Helper()
	addr := os.Getenv("FORGERAIL_TEST_TEMPORAL_ADDR")
	if addr == "" {
		t.Skip("FORGERAIL_TEST_TEMPORAL_ADDR not set")
	}
	c, err := client.Dial(client.Options{
		HostPort: addr,
		Logger:   tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	})
	if err != nil {
		t.Fatalf("dial temporal: %v", err)
	}
	t.Cleanup(c.Close)

	store := ledger.NewMemStore()
	fake := chain.NewFake(cfg)
	acts := &settlement.Activities{Store: store, Chain: fake, PollInterval: 20 * time.Millisecond, MaxWait: 5 * time.Second}

	queue := fmt.Sprintf("forgerail-test-%s-%d", strings.ToLower(t.Name()), time.Now().UnixNano())
	w := settlement.NewWorker(c, queue, acts, worker.Options{})
	if err := w.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(w.Stop)

	ctx := context.Background()
	user, _ := store.CreateAccount(ctx, ledger.NewAccount{Name: "alice"})
	if _, _, err := store.CreateTransfer(ctx, ledger.TransferRequest{
		IdempotencyKey: "fund", Kind: ledger.KindInternal,
		FromAccount: ledger.AccountOnchainIn, ToAccount: user.ID, Amount: money.USDC(100),
	}); err != nil {
		t.Fatal(err)
	}
	return &temporalEnv{client: c, starter: settlement.TemporalStarter{Client: c, TaskQueue: queue}, store: store, chain: fake, user: user.ID}
}

func (e *temporalEnv) withdraw(t *testing.T, key string, amt money.Amount) ledger.Transfer {
	t.Helper()
	tr, _, err := e.store.CreateTransfer(context.Background(), ledger.TransferRequest{
		IdempotencyKey: key, Kind: ledger.KindWithdrawal, FromAccount: e.user,
		Destination: "0x" + strings.Repeat("5a", 20), Amount: amt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// result waits for the workflow for transferID and returns its result plus
// the run id it finished in.
func (e *temporalEnv) result(t *testing.T, transferID string) (settlement.WithdrawalResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := e.client.GetWorkflow(ctx, settlement.WorkflowID(transferID), "")
	var res settlement.WithdrawalResult
	if err := run.Get(ctx, &res); err != nil {
		t.Fatalf("workflow for %s: %v", transferID, err)
	}
	return res, run.GetRunID()
}

func (e *temporalEnv) balance(t *testing.T, id string) money.Amount {
	t.Helper()
	a, err := e.store.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Balance
}

func TestTemporalDuplicateStartsShareOneRun(t *testing.T) {
	e := newTemporalEnv(t, chain.FakeConfig{ConfirmAfter: 100 * time.Millisecond})
	tr := e.withdraw(t, "wd-1", money.USDC(30))

	// 20 concurrent starts for the same transfer, like 20 retried API calls
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.starter.StartWithdrawal(context.Background(), tr.ID)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("start: %v", err)
		}
	}

	res, runID := e.result(t, tr.ID)
	if res.Status != ledger.StatusSettled {
		t.Fatalf("result %+v", res)
	}

	// starting again after it finished must not run it a second time
	for i := 0; i < 3; i++ {
		if err := e.starter.StartWithdrawal(context.Background(), tr.ID); err != nil {
			t.Fatalf("start after completion: %v", err)
		}
	}
	desc, err := e.client.DescribeWorkflowExecution(context.Background(), settlement.WorkflowID(tr.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := desc.GetWorkflowExecutionInfo().GetExecution().GetRunId(); got != runID {
		t.Fatalf("a new run was started: %s, first run %s", got, runID)
	}

	if e.chain.Broadcasts() != 1 {
		t.Errorf("tx reached the chain %d times", e.chain.Broadcasts())
	}
	if got := e.balance(t, e.user); got != money.USDC(70) {
		t.Errorf("user balance %s", got)
	}
	if got := e.balance(t, ledger.AccountOnchainOut); got != money.USDC(30) {
		t.Errorf("onchain_out %s", got)
	}
}

func TestTemporalRetriedAPIRequestsSettleOnce(t *testing.T) {
	e := newTemporalEnv(t, chain.FakeConfig{BroadcastFailRate: 0.5, Seed: 7, ConfirmAfter: 50 * time.Millisecond})
	svc := &service.Service{Store: e.store, Starter: e.starter}
	req := ledger.TransferRequest{
		IdempotencyKey: "api-wd", Kind: ledger.KindWithdrawal, FromAccount: e.user,
		Destination: "0x" + strings.Repeat("6b", 20), Amount: money.USDC(10),
	}

	ids := make([]string, 10)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr, _, err := svc.CreateTransfer(context.Background(), req)
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			ids[i] = tr.ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("same key gave two transfers: %s %s", ids[0], id)
		}
	}

	res, _ := e.result(t, ids[0])
	if res.Status != ledger.StatusSettled {
		t.Fatalf("result %+v", res)
	}
	if e.chain.Broadcasts() != 1 {
		t.Errorf("tx reached the chain %d times", e.chain.Broadcasts())
	}
	if got := e.balance(t, ledger.AccountOnchainOut); got != money.USDC(10) {
		t.Errorf("onchain_out %s (settled more than once?)", got)
	}
}

func TestTemporalRevertReleasesFunds(t *testing.T) {
	e := newTemporalEnv(t, chain.FakeConfig{RevertRate: 1})
	tr := e.withdraw(t, "wd-revert", money.USDC(40))
	if err := e.starter.StartWithdrawal(context.Background(), tr.ID); err != nil {
		t.Fatal(err)
	}
	res, _ := e.result(t, tr.ID)
	if res.Status != ledger.StatusFailed {
		t.Fatalf("result %+v", res)
	}
	if got := e.balance(t, e.user); got != money.USDC(100) {
		t.Errorf("user should be made whole, balance %s", got)
	}
	if got := e.balance(t, ledger.AccountSettlement); got != 0 {
		t.Errorf("settlement balance %s", got)
	}
}
