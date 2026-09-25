package settlement

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Amaan729/ForgeRail/internal/chain"
	"github.com/Amaan729/ForgeRail/internal/ledger"
)

// LocalRunner runs the same steps as WithdrawalWorkflow in a goroutine with
// plain retry loops. It lets you run the API and the load generator without
// a Temporal cluster.
//
// It is NOT durable: if the process dies mid-withdrawal the transfer stays
// pending/submitted until something starts it again. Use Temporal for
// anything that matters.
type LocalRunner struct {
	Acts     *Activities
	Attempts int           // per step, default 8
	Backoff  time.Duration // first retry delay, default 50ms (doubles, capped at 2s)
	Log      *slog.Logger

	wg       sync.WaitGroup
	inflight sync.Map
}

var _ Starter = (*LocalRunner)(nil)

// StartWithdrawal returns immediately. Calls for a transfer that is already
// being processed are ignored.
func (r *LocalRunner) StartWithdrawal(_ context.Context, transferID string) error {
	if _, busy := r.inflight.LoadOrStore(transferID, struct{}{}); busy {
		return nil
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.inflight.Delete(transferID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, err := r.Run(ctx, transferID); err != nil {
			r.logger().Error("withdrawal stuck", "transfer", transferID, "error", err)
		}
	}()
	return nil
}

// Wait blocks until every started withdrawal has finished.
func (r *LocalRunner) Wait() { r.wg.Wait() }

// Run processes one withdrawal synchronously. Mirrors WithdrawalWorkflow;
// keep the two in sync.
func (r *LocalRunner) Run(ctx context.Context, transferID string) (WithdrawalResult, error) {
	a := r.Acts
	var w WithdrawalInfo
	if err := r.retry(ctx, func() (err error) { w, err = a.LoadWithdrawal(ctx, transferID); return }); err != nil {
		return WithdrawalResult{}, err
	}
	if w.Status.Terminal() {
		return WithdrawalResult{Status: w.Status, TxHash: w.TxHash}, nil
	}

	release := func(reason string) (WithdrawalResult, error) {
		if err := r.retry(ctx, func() error { return a.Release(ctx, w.TransferID, reason) }); err != nil {
			return WithdrawalResult{}, err
		}
		return WithdrawalResult{Status: ledger.StatusFailed, TxHash: w.TxHash, FailureReason: reason}, nil
	}

	if w.TxHash == "" {
		var tx chain.SignedTx
		if err := r.retry(ctx, func() (err error) { tx, err = a.PrepareTx(ctx, w); return }); err != nil {
			return release("could not prepare transaction: " + errMessage(err))
		}
		if err := r.retry(ctx, func() error { return a.MarkSubmitted(ctx, w.TransferID, tx.Hash) }); err != nil {
			return WithdrawalResult{}, err
		}
		w.TxHash = tx.Hash
		if err := r.retry(ctx, func() error { return a.Broadcast(ctx, tx) }); err != nil {
			r.logger().Warn("broadcast failed, checking chain", "transfer", w.TransferID, "error", err)
		}
	}

	var rc chain.Receipt
	err := r.retry(ctx, func() (err error) { rc, err = a.WaitForReceipt(ctx, w.TxHash); return })
	switch {
	case err != nil && errType(err) == ErrTypeTxNotFound:
		return release("transaction never reached the chain")
	case err != nil:
		return WithdrawalResult{Status: ledger.StatusSubmitted, TxHash: w.TxHash}, err
	case rc.Status == chain.TxReverted:
		return release("transaction reverted on chain")
	}
	if err := r.retry(ctx, func() error { return a.Settle(ctx, w.TransferID) }); err != nil {
		return WithdrawalResult{}, err
	}
	return WithdrawalResult{Status: ledger.StatusSettled, TxHash: w.TxHash}, nil
}

func (r *LocalRunner) retry(ctx context.Context, fn func() error) error {
	attempts := r.Attempts
	if attempts <= 0 {
		attempts = 8
	}
	delay := r.Backoff
	if delay <= 0 {
		delay = 50 * time.Millisecond
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil || nonRetryable(err) {
			return err
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Second)
	}
	return err
}

func (r *LocalRunner) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}
