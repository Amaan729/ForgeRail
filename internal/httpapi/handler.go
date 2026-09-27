// Package httpapi is the REST API described in api/openapi.yaml.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Amaan729/ForgeRail/api"
	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
	"github.com/Amaan729/ForgeRail/internal/service"
)

// Route is one method+path pair. The contract test checks this list
// against the OpenAPI spec so the two can't drift apart.
type Route struct {
	Method, Path string
}

var Routes = []Route{
	{"GET", "/healthz"},
	{"POST", "/v1/accounts"},
	{"GET", "/v1/accounts/{id}"},
	{"GET", "/v1/accounts/{id}/entries"},
	{"POST", "/v1/transfers"},
	{"GET", "/v1/transfers/{id}"},
}

type handler struct {
	svc *service.Service
	log *slog.Logger
}

// New returns the REST handler. Also serves the spec at /openapi.yaml.
func New(svc *service.Service, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &handler{svc: svc, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(api.OpenAPI)
	})
	mux.HandleFunc("POST /v1/accounts", h.createAccount)
	mux.HandleFunc("GET /v1/accounts/{id}", h.getAccount)
	mux.HandleFunc("GET /v1/accounts/{id}/entries", h.listEntries)
	mux.HandleFunc("POST /v1/transfers", h.createTransfer)
	mux.HandleFunc("GET /v1/transfers/{id}", h.getTransfer)
	return h.middleware(mux)
}

// --- JSON shapes (see openapi.yaml) ---

type accountJSON struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Balance   string    `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
}

type transferJSON struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Kind           string    `json:"kind"`
	FromAccountID  string    `json:"from_account_id"`
	ToAccountID    string    `json:"to_account_id,omitempty"`
	Destination    string    `json:"destination_address,omitempty"`
	Amount         string    `json:"amount"`
	Memo           string    `json:"memo,omitempty"`
	Status         string    `json:"status"`
	FailureReason  string    `json:"failure_reason,omitempty"`
	TxHash         string    `json:"tx_hash,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type entryJSON struct {
	ID         int64     `json:"id"`
	TransferID string    `json:"transfer_id"`
	AccountID  string    `json:"account_id"`
	Direction  string    `json:"direction"`
	Amount     string    `json:"amount"`
	Phase      string    `json:"phase"`
	CreatedAt  time.Time `json:"created_at"`
}

type createTransferJSON struct {
	Kind          string `json:"kind"`
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	Destination   string `json:"destination_address"`
	Amount        string `json:"amount"`
	Memo          string `json:"memo"`
}

func toAccountJSON(a ledger.Account) accountJSON {
	return accountJSON{ID: a.ID, Name: a.Name, Balance: a.Balance.String(), CreatedAt: a.CreatedAt}
}

func toTransferJSON(t ledger.Transfer) transferJSON {
	return transferJSON{
		ID: t.ID, IdempotencyKey: t.IdempotencyKey, Kind: string(t.Kind),
		FromAccountID: t.FromAccount, ToAccountID: t.ToAccount, Destination: t.Destination,
		Amount: t.Amount.String(), Memo: t.Memo, Status: string(t.Status),
		FailureReason: t.FailureReason, TxHash: t.TxHash, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// --- handlers ---

func (h *handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		h.fail(w, err)
		return
	}
	a, err := h.svc.CreateAccount(r.Context(), body.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAccountJSON(a))
}

func (h *handler) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := h.svc.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAccountJSON(a))
}

func (h *handler) listEntries(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 1000 {
			h.fail(w, fmt.Errorf("%w: limit must be between 1 and 1000", ledger.ErrInvalidRequest))
			return
		}
		limit = n
	}
	entries, err := h.svc.ListEntries(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]entryJSON, len(entries))
	for i, e := range entries {
		out[i] = entryJSON{
			ID: e.ID, TransferID: e.TransferID, AccountID: e.AccountID, Direction: string(e.Direction),
			Amount: e.Amount.String(), Phase: string(e.Phase), CreatedAt: e.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

func (h *handler) createTransfer(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}
	var body createTransferJSON
	if err := decode(r, &body); err != nil {
		h.fail(w, err)
		return
	}
	amt, err := money.Parse(body.Amount)
	if err != nil {
		h.fail(w, fmt.Errorf("%w: amount: %v", ledger.ErrInvalidRequest, err))
		return
	}
	t, replayed, err := h.svc.CreateTransfer(r.Context(), ledger.TransferRequest{
		IdempotencyKey: key,
		Kind:           ledger.TransferKind(body.Kind),
		FromAccount:    body.FromAccountID,
		ToAccount:      body.ToAccountID,
		Destination:    body.Destination,
		Amount:         amt,
		Memo:           body.Memo,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	code := http.StatusCreated
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		code = http.StatusOK
	}
	writeJSON(w, code, toTransferJSON(t))
}

func (h *handler) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.GetTransfer(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransferJSON(t))
}

// --- plumbing ---

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: bad JSON body: %v", ledger.ErrInvalidRequest, err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": errCode, "message": msg}})
}

func (h *handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, ledger.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, service.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, ledger.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
	case errors.Is(err, ledger.ErrInvalidState), errors.Is(err, ledger.ErrAccountExists):
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
	case errors.Is(err, service.ErrSettlementUnavailable):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "settlement_unavailable", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "internal", "request timed out")
	default:
		h.log.Error("http: internal error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// middleware adds panic recovery and a debug log line per request.
func (h *handler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				h.log.Error("http: panic", "panic", p, "path", r.URL.Path)
				writeError(rec, http.StatusInternalServerError, "internal", "internal error")
			}
			h.log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.code, "took", time.Since(start))
		}()
		next.ServeHTTP(rec, r)
	})
}
