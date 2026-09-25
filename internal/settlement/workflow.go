package settlement

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Amaan729/ForgeRail/internal/chain"
	"github.com/Amaan729/ForgeRail/internal/ledger"
)

const TaskQueue = "forgerail-settlement"

type WithdrawalInput struct {
	TransferID string
}

type WithdrawalResult struct {
	Status        ledger.TransferStatus
	TxHash        string
	FailureReason string
}

// WorkflowID is deterministic per transfer, so starting the workflow twice
// for the same withdrawal attaches to the existing run instead.
func WorkflowID(transferID string) string { return "withdrawal-" + transferID }

// WithdrawalWorkflow drives one withdrawal to settled or failed.
//
//	load -> prepare (sign) -> mark submitted -> broadcast -> wait for receipt -> settle
//	                 \ permanent error                            \ reverted / never seen
//	                  `-> release                                  `-> release
//
// The signed tx lives in workflow history, so if the worker dies after
// signing, the new worker re-broadcasts the exact same bytes.
func WithdrawalWorkflow(ctx workflow.Context, in WithdrawalInput) (WithdrawalResult, error) {
	log := workflow.GetLogger(ctx)
	var a *Activities

	dbCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        200 * time.Millisecond,
			BackoffCoefficient:     2,
			MaximumInterval:        10 * time.Second,
			NonRetryableErrorTypes: []string{ErrTypeInvalidState},
		},
	})
	chainCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        time.Second,
			BackoffCoefficient:     2,
			MaximumInterval:        30 * time.Second,
			MaximumAttempts:        10,
			NonRetryableErrorTypes: []string{ErrTypeChainPermanent},
		},
	})
	waitCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		HeartbeatTimeout:    time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        5 * time.Second,
			MaximumAttempts:        5,
			NonRetryableErrorTypes: []string{ErrTypeTxNotFound},
		},
	})

	var w WithdrawalInfo
	if err := workflow.ExecuteActivity(dbCtx, a.LoadWithdrawal, in.TransferID).Get(ctx, &w); err != nil {
		return WithdrawalResult{}, err
	}
	if w.Status.Terminal() {
		return WithdrawalResult{Status: w.Status, TxHash: w.TxHash}, nil
	}

	release := func(reason string) (WithdrawalResult, error) {
		log.Warn("releasing withdrawal", "transfer", w.TransferID, "reason", reason)
		if err := workflow.ExecuteActivity(dbCtx, a.Release, w.TransferID, reason).Get(ctx, nil); err != nil {
			return WithdrawalResult{}, err
		}
		return WithdrawalResult{Status: ledger.StatusFailed, TxHash: w.TxHash, FailureReason: reason}, nil
	}

	if w.TxHash == "" {
		var tx chain.SignedTx
		if err := workflow.ExecuteActivity(chainCtx, a.PrepareTx, w).Get(ctx, &tx); err != nil {
			return release("could not prepare transaction: " + errMessage(err))
		}
		// Persist the hash before broadcasting so we never lose track of a
		// tx that might be on chain.
		if err := workflow.ExecuteActivity(dbCtx, a.MarkSubmitted, w.TransferID, tx.Hash).Get(ctx, nil); err != nil {
			return WithdrawalResult{}, err
		}
		w.TxHash = tx.Hash
		if err := workflow.ExecuteActivity(chainCtx, a.Broadcast, tx).Get(ctx, nil); err != nil {
			// The node may have accepted it anyway. The receipt decides.
			log.Warn("broadcast failed, checking chain", "transfer", w.TransferID, "error", err)
		}
	}

	var rc chain.Receipt
	err := workflow.ExecuteActivity(waitCtx, a.WaitForReceipt, w.TxHash).Get(ctx, &rc)
	switch {
	case err != nil && errType(err) == ErrTypeTxNotFound:
		return release("transaction never reached the chain")
	case err != nil:
		// RPC trouble etc. We don't know where the tx is, so don't guess:
		// fail the workflow and leave the transfer submitted for a human.
		return WithdrawalResult{Status: ledger.StatusSubmitted, TxHash: w.TxHash}, err
	case rc.Status == chain.TxReverted:
		return release("transaction reverted on chain")
	}

	if err := workflow.ExecuteActivity(dbCtx, a.Settle, w.TransferID).Get(ctx, nil); err != nil {
		return WithdrawalResult{}, err
	}
	return WithdrawalResult{Status: ledger.StatusSettled, TxHash: w.TxHash}, nil
}

// errMessage digs the original message out of Temporal's ActivityError wrapper.
func errMessage(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return appErr.Error()
	}
	return err.Error()
}
