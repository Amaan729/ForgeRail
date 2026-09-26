// Package service is the thin layer both APIs (gRPC and REST) call into. It
// owns the rules that aren't about storage: who may move money out of system
// accounts, and kicking off settlement for withdrawals.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/settlement"
)

var (
	// ErrSettlementUnavailable means the transfer was recorded (and funds
	// held) but settlement could not be started. Retrying with the same
	// idempotency key is safe and will try to start it again.
	ErrSettlementUnavailable = errors.New("transfer recorded but settlement could not be started, retry with the same idempotency key")

	// ErrForbidden is returned for transfers out of system accounts.
	ErrForbidden = errors.New("transfers from system accounts are not allowed")
)

type Service struct {
	Store   ledger.Store
	Starter settlement.Starter
	Log     *slog.Logger

	// AllowSystemTransfers lets callers move money out of sys_* accounts,
	// which is how dev mode, the replay tool and the load generator fund
	// test accounts. In a real deployment deposits would come from a chain
	// watcher instead and this stays off.
	AllowSystemTransfers bool
}

func (s *Service) CreateAccount(ctx context.Context, name string) (ledger.Account, error) {
	if len(name) > 200 {
		return ledger.Account{}, fmt.Errorf("%w: name is too long", ledger.ErrInvalidRequest)
	}
	return s.Store.CreateAccount(ctx, ledger.NewAccount{Name: name})
}

func (s *Service) GetAccount(ctx context.Context, id string) (ledger.Account, error) {
	return s.Store.GetAccount(ctx, id)
}

func (s *Service) GetTransfer(ctx context.Context, id string) (ledger.Transfer, error) {
	return s.Store.GetTransfer(ctx, id)
}

func (s *Service) ListEntries(ctx context.Context, accountID string, limit int) ([]ledger.Entry, error) {
	return s.Store.ListEntries(ctx, accountID, limit)
}

// CreateTransfer records the transfer and, for withdrawals that are still in
// flight, (re)starts settlement. Starting is idempotent, so a client retry
// after a crash between "hold posted" and "workflow started" heals itself.
func (s *Service) CreateTransfer(ctx context.Context, req ledger.TransferRequest) (ledger.Transfer, bool, error) {
	if !s.AllowSystemTransfers && strings.HasPrefix(req.FromAccount, "sys_") {
		return ledger.Transfer{}, false, ErrForbidden
	}
	t, replayed, err := s.Store.CreateTransfer(ctx, req)
	if err != nil {
		return t, replayed, err
	}
	if t.Kind == ledger.KindWithdrawal && !t.Status.Terminal() && s.Starter != nil {
		if err := s.Starter.StartWithdrawal(ctx, t.ID); err != nil {
			s.logger().Error("start settlement", "transfer", t.ID, "error", err)
			return t, replayed, fmt.Errorf("%w: %v", ErrSettlementUnavailable, err)
		}
	}
	return t, replayed, nil
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
