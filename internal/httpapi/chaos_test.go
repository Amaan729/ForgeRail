package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChaosLostResponseStillRunsHandler(t *testing.T) {
	var handled atomic.Int32
	h := Chaos(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
		w.WriteHeader(http.StatusCreated)
	}), 0, 1)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/transfers", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable || handled.Load() != 1 {
		t.Fatalf("code=%d handled=%d", rec.Code, handled.Load())
	}

	// GETs and other POSTs are never touched
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/transfers/x", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("GET code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/accounts", strings.NewReader("{}")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/accounts code=%d", rec.Code)
	}
}

func TestChaosErrorSkipsHandler(t *testing.T) {
	var handled atomic.Int32
	h := Chaos(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handled.Add(1) }), 1, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/transfers", nil))
	if rec.Code != http.StatusServiceUnavailable || handled.Load() != 0 {
		t.Fatalf("code=%d handled=%d", rec.Code, handled.Load())
	}
}
