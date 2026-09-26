package grpcapi

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	forgerailv1 "github.com/Amaan729/ForgeRail/gen/forgerail/v1"
	"github.com/Amaan729/ForgeRail/internal/ledger"
)

var kindFromPB = map[forgerailv1.TransferKind]ledger.TransferKind{
	forgerailv1.TransferKind_TRANSFER_KIND_INTERNAL:   ledger.KindInternal,
	forgerailv1.TransferKind_TRANSFER_KIND_WITHDRAWAL: ledger.KindWithdrawal,
}

var kindToPB = map[ledger.TransferKind]forgerailv1.TransferKind{
	ledger.KindInternal:   forgerailv1.TransferKind_TRANSFER_KIND_INTERNAL,
	ledger.KindWithdrawal: forgerailv1.TransferKind_TRANSFER_KIND_WITHDRAWAL,
}

var statusToPB = map[ledger.TransferStatus]forgerailv1.TransferStatus{
	ledger.StatusPosted:    forgerailv1.TransferStatus_TRANSFER_STATUS_POSTED,
	ledger.StatusPending:   forgerailv1.TransferStatus_TRANSFER_STATUS_PENDING,
	ledger.StatusSubmitted: forgerailv1.TransferStatus_TRANSFER_STATUS_SUBMITTED,
	ledger.StatusSettled:   forgerailv1.TransferStatus_TRANSFER_STATUS_SETTLED,
	ledger.StatusFailed:    forgerailv1.TransferStatus_TRANSFER_STATUS_FAILED,
	ledger.StatusRejected:  forgerailv1.TransferStatus_TRANSFER_STATUS_REJECTED,
}

func toPBAccount(a ledger.Account) *forgerailv1.Account {
	return &forgerailv1.Account{
		Id:            a.ID,
		Name:          a.Name,
		BalanceMicros: a.Balance.Micros(),
		AllowNegative: a.AllowNegative,
		CreatedAt:     timestamppb.New(a.CreatedAt),
	}
}

func toPBTransfer(t ledger.Transfer) *forgerailv1.Transfer {
	return &forgerailv1.Transfer{
		Id:                 t.ID,
		IdempotencyKey:     t.IdempotencyKey,
		Kind:               kindToPB[t.Kind],
		FromAccountId:      t.FromAccount,
		ToAccountId:        t.ToAccount,
		DestinationAddress: t.Destination,
		AmountMicros:       t.Amount.Micros(),
		Memo:               t.Memo,
		Status:             statusToPB[t.Status],
		FailureReason:      t.FailureReason,
		TxHash:             t.TxHash,
		CreatedAt:          timestamppb.New(t.CreatedAt),
		UpdatedAt:          timestamppb.New(t.UpdatedAt),
	}
}

func toPBEntry(e ledger.Entry) *forgerailv1.Entry {
	dir := forgerailv1.EntryDirection_ENTRY_DIRECTION_DEBIT
	if e.Direction == ledger.Credit {
		dir = forgerailv1.EntryDirection_ENTRY_DIRECTION_CREDIT
	}
	return &forgerailv1.Entry{
		Id:           e.ID,
		TransferId:   e.TransferID,
		AccountId:    e.AccountID,
		Direction:    dir,
		AmountMicros: e.Amount.Micros(),
		Phase:        string(e.Phase),
		CreatedAt:    timestamppb.New(e.CreatedAt),
	}
}
