// Package settlement moves withdrawals from "funds held" to "settled on
// Base" (or back to the user if the chain says no).
//
// The real thing runs as a Temporal workflow (WithdrawalWorkflow). For local
// dev and the load generator there is also LocalRunner, which runs the same
// activities in a goroutine without any durability.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/Amaan729/ForgeRail/internal/chain"
	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
)

// Application error types. The workflow's retry policies refuse to retry
// these.
const (
	ErrTypeInvalidState   = "InvalidState"
	ErrTypeChainPermanent = "ChainPermanent"
	ErrTypeTxNotFound     = "TxNotFound"
)

type Activities struct {
	Store ledger.Store
	Chain chain.Client

	// PollInterval is how often WaitForReceipt asks the chain. Default 2s.
	PollInterval time.Duration
	// MaxWait is how long WaitForReceipt waits for a tx that the chain has
	// never seen before giving up. Default 10 minutes.
	MaxWait time.Duration
}

// WithdrawalInfo is the slice of a transfer the workflow needs.
type WithdrawalInfo struct {
	TransferID   string
	Status       ledger.TransferStatus
	Destination  string
	AmountMicros int64
	TxHash       string
}

func (a *Activities) LoadWithdrawal(ctx context.Context, transferID string) (WithdrawalInfo, error) {
	t, err := a.Store.GetTransfer(ctx, transferID)
	if err != nil {
		return WithdrawalInfo{}, wrap(err)
	}
	if t.Kind != ledger.KindWithdrawal {
		return WithdrawalInfo{}, wrap(fmt.Errorf("%w: %s is not a withdrawal", ledger.ErrInvalidState, transferID))
	}
	return WithdrawalInfo{
		TransferID:   t.ID,
		Status:       t.Status,
		Destination:  t.Destination,
		AmountMicros: t.Amount.Micros(),
		TxHash:       t.TxHash,
	}, nil
}

func (a *Activities) PrepareTx(ctx context.Context, w WithdrawalInfo) (chain.SignedTx, error) {
	tx, err := a.Chain.PrepareUSDCTransfer(ctx, w.TransferID, w.Destination, money.FromMicros(w.AmountMicros))
	return tx, wrap(err)
}

func (a *Activities) MarkSubmitted(ctx context.Context, transferID, txHash string) error {
	_, err := a.Store.MarkSubmitted(ctx, transferID, txHash)
	return wrap(err)
}

func (a *Activities) Broadcast(ctx context.Context, tx chain.SignedTx) error {
	return wrap(a.Chain.Broadcast(ctx, tx))
}

// WaitForReceipt polls until the tx is confirmed or reverted. If the chain
// still has never heard of the tx after MaxWait it gives up with a
// non-retryable TxNotFound so the workflow can release the hold.
func (a *Activities) WaitForReceipt(ctx context.Context, txHash string) (chain.Receipt, error) {
	poll := a.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	maxWait := a.MaxWait
	if maxWait <= 0 {
		maxWait = 10 * time.Minute
	}
	deadline := time.Now().Add(maxWait)
	var last chain.Receipt
	for {
		rc, err := a.Chain.Receipt(ctx, txHash)
		if err == nil {
			last = rc
			if rc.Status == chain.TxConfirmed || rc.Status == chain.TxReverted {
				return rc, nil
			}
		}
		heartbeat(ctx, string(last.Status))
		if time.Now().After(deadline) {
			if last.Status == chain.TxNotFound || last.Status == "" {
				return last, temporal.NewNonRetryableApplicationError(
					fmt.Sprintf("tx %s not seen on chain after %s", txHash, maxWait), ErrTypeTxNotFound, err)
			}
			// It's on chain but not final yet: let the retry policy call us again.
			return last, fmt.Errorf("tx %s still %s after %s", txHash, last.Status, maxWait)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (a *Activities) Settle(ctx context.Context, transferID string) error {
	_, err := a.Store.Settle(ctx, transferID)
	return wrap(err)
}

func (a *Activities) Release(ctx context.Context, transferID, reason string) error {
	_, err := a.Store.Release(ctx, transferID, reason)
	return wrap(err)
}

// wrap turns errors that will never succeed on retry into non-retryable
// Temporal application errors.
func wrap(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ledger.ErrInvalidState), errors.Is(err, ledger.ErrNotFound):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeInvalidState, err)
	case errors.Is(err, chain.ErrPermanent):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeChainPermanent, err)
	}
	return err
}

func nonRetryable(err error) bool {
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.NonRetryable()
}

func errType(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return appErr.Type()
	}
	return ""
}

// heartbeat is a no-op outside Temporal (LocalRunner calls activities directly).
func heartbeat(ctx context.Context, details ...any) {
	if activity.IsActivity(ctx) {
		activity.RecordHeartbeat(ctx, details...)
	}
}
