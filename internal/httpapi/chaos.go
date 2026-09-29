package httpapi

import (
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
)

// Chaos wraps a handler and breaks POST /v1/transfers on purpose, for load
// tests (other routes aren't idempotent, so breaking them proves nothing):
//
//   - errRate: fail with 503 before the request is handled
//   - lostRate: handle the request (so it commits!) but throw the response
//     away and send a 503, like a dropped connection after the server did
//     the work. Clients that retry with the same idempotency key must not
//     double-post.
//
// Never enable this outside a test environment.
func Chaos(next http.Handler, errRate, lostRate float64) http.Handler {
	if errRate <= 0 && lostRate <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/transfers" {
			next.ServeHTTP(w, r)
			return
		}
		roll := rand.Float64()
		switch {
		case roll < errRate:
			writeError(w, http.StatusServiceUnavailable, "internal", "chaos: injected failure")
		case roll < errRate+lostRate:
			next.ServeHTTP(httptest.NewRecorder(), r)
			writeError(w, http.StatusServiceUnavailable, "internal", "chaos: response lost")
		default:
			next.ServeHTTP(w, r)
		}
	})
}
