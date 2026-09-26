// Package grpcapi exposes the ledger over gRPC (forgerail.v1.LedgerService).
package grpcapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	forgerailv1 "github.com/Amaan729/ForgeRail/gen/forgerail/v1"
	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
	"github.com/Amaan729/ForgeRail/internal/service"
)

type Server struct {
	forgerailv1.UnimplementedLedgerServiceServer
	Svc *service.Service
}

// Register attaches the ledger service to a grpc.Server.
func Register(g *grpc.Server, svc *service.Service) {
	forgerailv1.RegisterLedgerServiceServer(g, &Server{Svc: svc})
}

func (s *Server) CreateAccount(ctx context.Context, req *forgerailv1.CreateAccountRequest) (*forgerailv1.CreateAccountResponse, error) {
	a, err := s.Svc.CreateAccount(ctx, req.GetName())
	if err != nil {
		return nil, toStatus(err)
	}
	return &forgerailv1.CreateAccountResponse{Account: toPBAccount(a)}, nil
}

func (s *Server) GetAccount(ctx context.Context, req *forgerailv1.GetAccountRequest) (*forgerailv1.GetAccountResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	a, err := s.Svc.GetAccount(ctx, req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &forgerailv1.GetAccountResponse{Account: toPBAccount(a)}, nil
}

func (s *Server) CreateTransfer(ctx context.Context, req *forgerailv1.CreateTransferRequest) (*forgerailv1.CreateTransferResponse, error) {
	kind, ok := kindFromPB[req.GetKind()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "kind must be INTERNAL or WITHDRAWAL")
	}
	t, replayed, err := s.Svc.CreateTransfer(ctx, ledger.TransferRequest{
		IdempotencyKey: req.GetIdempotencyKey(),
		Kind:           kind,
		FromAccount:    req.GetFromAccountId(),
		ToAccount:      req.GetToAccountId(),
		Destination:    req.GetDestinationAddress(),
		Amount:         money.FromMicros(req.GetAmountMicros()),
		Memo:           req.GetMemo(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &forgerailv1.CreateTransferResponse{Transfer: toPBTransfer(t), Replayed: replayed}, nil
}

func (s *Server) GetTransfer(ctx context.Context, req *forgerailv1.GetTransferRequest) (*forgerailv1.GetTransferResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	t, err := s.Svc.GetTransfer(ctx, req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &forgerailv1.GetTransferResponse{Transfer: toPBTransfer(t)}, nil
}

func (s *Server) ListEntries(ctx context.Context, req *forgerailv1.ListEntriesRequest) (*forgerailv1.ListEntriesResponse, error) {
	entries, err := s.Svc.ListEntries(ctx, req.GetAccountId(), int(req.GetLimit()))
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*forgerailv1.Entry, len(entries))
	for i, e := range entries {
		out[i] = toPBEntry(e)
	}
	return &forgerailv1.ListEntriesResponse{Entries: out}, nil
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, ledger.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ledger.ErrInvalidRequest):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ledger.ErrAccountExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ledger.ErrIdempotencyConflict), errors.Is(err, ledger.ErrInvalidState):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, service.ErrForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, service.ErrSettlementUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	slog.Error("grpc: internal error", "error", err)
	return status.Error(codes.Internal, "internal error")
}

// LoggingInterceptor logs one line per call.
func LoggingInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		log.Debug("grpc", "method", info.FullMethod, "code", status.Code(err).String(), "took", time.Since(start))
		return resp, err
	}
}
