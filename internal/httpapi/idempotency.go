package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"time"

	"github.com/razi-king/booking-engine/internal/store"
)

// HeaderIdemKey is the request header clients send; same name Stripe uses.
const HeaderIdemKey = "Idempotency-Key"

// maxBody caps request bodies at 64 KB. Without a cap, one client could send
// gigabytes and exhaust the server's memory.
const maxBody = 64 << 10 // << is a bit shift: 64 × 2^10 = 65,536

// WithIdempotency makes a POST handler safe to retry.
//
// HOW IT WORKS
//  1. No Idempotency-Key header → run the handler normally.
//  2. Read the body and compute a FINGERPRINT: SHA-256 of method, path, user
//     and body. Same key + same fingerprint = the same request again.
//  3. store.Reserve(key, fingerprint):
//     - first time → run the handler, RECORD its response, save it with Complete.
//     - seen and finished → REPLAY the saved response; the handler does NOT run.
//     - still running → 409; different body → 422 (via writeError).
//  4. If the handler failed with a 5xx, Forget the key so a retry really retries.
//
// The key is scoped to the user ("user:key"), so two users who happen to pick
// the same key never see each other's responses.
func WithIdempotency(s store.IdempotencyStore, now func() time.Time) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(HeaderIdemKey)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > 255 {
				writeJSON(w, http.StatusBadRequest, ErrorBody{Error: "validation_failed", Message: "Idempotency-Key is too long"})
				return
			}

			// Read the whole body (capped), then put it back so the handler can read it too.
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
			if err != nil {
				writeError(w, err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			user := r.Header.Get(HeaderUser)
			sum := sha256.Sum256([]byte(r.Method + "\n" + r.URL.Path + "\n" + user + "\n" + string(body)))
			fingerprint := hex.EncodeToString(sum[:])
			scoped := user + ":" + key

			saved, err := s.Reserve(r.Context(), scoped, fingerprint, now())
			if err != nil {
				writeError(w, err)
				return
			}
			if saved != nil { // seen before: replay, do NOT run the handler again
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(saved.Status)
				w.Write(saved.Body)
				return
			}

			// First time: run the handler, capturing what it writes.
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// Use a fresh context: if the client hung up, r.Context() is
			// cancelled, but we still want to save the outcome for its retry.
			ctx := context.WithoutCancel(r.Context())
			if rec.status >= 500 {
				_ = s.Forget(ctx, scoped) // server error: allow a genuine retry
				return
			}
			_ = s.Complete(ctx, scoped, store.SavedResponse{Status: rec.status, Body: rec.buf.Bytes()})
		})
	}
}

// recorder passes the response through to the client AND keeps a copy.
// EMBEDDING http.ResponseWriter means we only override what we need.
type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.buf.Write(b) // keep a copy for replays
	return r.ResponseWriter.Write(b)
}
