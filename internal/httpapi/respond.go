// Package httpapi turns HTTP requests into service calls and results into JSON.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/razi-king/booking-engine/internal/domain"
	"github.com/razi-king/booking-engine/internal/store"
)

// ErrorBody is the shape of EVERY error response, so clients parse one format.
type ErrorBody struct {
	Error   string            `json:"error"`            // stable machine code, e.g. "seat_unavailable"
	Message string            `json:"message"`          // human sentence
	Fields  map[string]string `json:"fields,omitempty"` // per-field validation problems
	Seats   []string          `json:"seats,omitempty"`  // which seats were taken
}

// writeJSON is the ONLY place a success body is written (DRY): same headers,
// same encoding everywhere.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}

// errorMapping is a TABLE from domain error to HTTP status and code.
//
// WHY A TABLE AND NOT A SWITCH IN EACH HANDLER? Eight handlers × seven
// errors would be 56 places to keep consistent. One table is DRY, and adding
// an error means adding one line here.
var errorMapping = []struct {
	err    error
	status int
	code   string
}{
	{domain.ErrInvalid, http.StatusBadRequest, "validation_failed"},
	{domain.ErrNotFound, http.StatusNotFound, "not_found"},
	{domain.ErrSeatTaken, http.StatusConflict, "seat_unavailable"},
	{domain.ErrHoldExpired, http.StatusGone, "hold_expired"}, // 410 Gone: it existed but is over
	{domain.ErrHoldNotActive, http.StatusConflict, "hold_not_active"},
	{domain.ErrAlreadyCancel, http.StatusConflict, "already_cancelled"},
	{store.ErrKeyMismatch, http.StatusUnprocessableEntity, "idempotency_key_reused"},
	{store.ErrKeyInProgress, http.StatusConflict, "request_in_progress"},
}

// writeError maps any error to a status and JSON body.
// Unknown errors become 500 with a GENERIC message: internal details (SQL
// text, hostnames) must never leak to clients. The real error goes to the log.
func writeError(w http.ResponseWriter, err error) {
	for _, m := range errorMapping {
		if errors.Is(err, m.err) {
			body := ErrorBody{Error: m.code, Message: err.Error()}
			var verr *domain.ValidationError
			if errors.As(err, &verr) {
				body.Fields, body.Message = verr.Fields, "Please fix the highlighted fields."
			}
			var sue *domain.SeatsUnavailableError
			if errors.As(err, &sue) {
				body.Seats = sue.Seats
			}
			writeJSON(w, m.status, body)
			return
		}
	}
	slog.Error("internal error", "err", err)
	writeJSON(w, http.StatusInternalServerError, ErrorBody{Error: "internal", Message: "Something went wrong. Please try again."})
}
