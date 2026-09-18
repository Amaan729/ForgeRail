// Package chain talks to Base (an Ethereum L2) to move USDC on-chain.
//
// Sending is split in two so it can be retried safely:
//
//  1. Prepare signs a transaction. The hash is known before anything is
//     broadcast, so the caller can persist it first.
//  2. Broadcast sends the signed bytes. Re-broadcasting the same signed tx is
//     harmless (same nonce, same hash), so it can be retried freely.
package chain

import (
	"context"
	"errors"

	"github.com/Amaan729/ForgeRail/internal/money"
)

// ErrPermanent wraps failures that will never succeed on retry (bad address,
// hot wallet out of funds, ...).
var ErrPermanent = errors.New("chain: permanent failure")

type TxStatus string

const (
	TxNotFound  TxStatus = "not_found"
	TxPending   TxStatus = "pending"   // seen but not enough confirmations yet
	TxConfirmed TxStatus = "confirmed" // mined, succeeded, enough confirmations
	TxReverted  TxStatus = "reverted"  // mined but the call failed
)

type SignedTx struct {
	Hash string
	Raw  []byte
}

type Receipt struct {
	TxHash        string
	Status        TxStatus
	BlockNumber   uint64
	Confirmations uint64
}

type Client interface {
	// PrepareUSDCTransfer signs an ERC-20 transfer of amount to address `to`.
	// ref is ForgeRail's transfer id, used for logging and by the fake chain.
	PrepareUSDCTransfer(ctx context.Context, ref, to string, amount money.Amount) (SignedTx, error)
	// Broadcast submits a signed tx. Must be idempotent.
	Broadcast(ctx context.Context, tx SignedTx) error
	// Receipt reports where a tx is.
	Receipt(ctx context.Context, txHash string) (Receipt, error)
}
