package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// HeaderUser identifies the caller.
//
// DEMO SIMPLIFICATION: in production the user id comes from a verified JWT,
// checked by an API gateway (that is Project 4). Here the gateway's job is
// simulated by trusting this header.
const HeaderUser = "X-User-ID"

// Middleware wraps a handler with extra behaviour.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so the FIRST listed runs FIRST (outermost).
// It loops backwards because wrapping is inside-out.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// WithRequestID gives every request an id (reusing the client's if sent).
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

// statusWriter remembers the status code for the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }

// WithLogging writes one structured JSON log line per request.
func WithLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("http request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"durationMs", time.Since(start).Milliseconds(), "requestId", w.Header().Get("X-Request-ID"))
	})
}

// WithRecover turns a panic into a 500 instead of a dropped connection.
func WithRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("panic in handler", "panic", p, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, ErrorBody{Error: "internal", Message: "Something went wrong."})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// WithCORS lets browser pages on the listed origins call the API.
func WithCORS(origins []string) Middleware {
	allowed := make(map[string]bool, len(origins)) // set for O(1) checks
	for _, o := range origins {
		allowed[strings.TrimSpace(o)] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o := r.Header.Get("Origin"); o != "" && (allowed["*"] || allowed[o]) {
				w.Header().Set("Access-Control-Allow-Origin", o)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-User-ID, Idempotency-Key")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			}
			if r.Method == http.MethodOptions { // browser "pre-flight" check: answer and stop
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
