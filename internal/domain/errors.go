package domain

import (
	"errors"
	"sort"
	"strings"
)

// SENTINEL ERRORS. Every layer returns these (possibly wrapped with extra
// detail using fmt.Errorf("...: %w", err)). The HTTP layer maps each one to a
// status code in ONE place (httpapi/errors.go). Callers check them with
// errors.Is, which also sees through wrapping.
var (
	ErrNotFound      = errors.New("not found")
	ErrSeatTaken     = errors.New("one or more seats are not available")
	ErrHoldExpired   = errors.New("hold has expired")
	ErrHoldNotActive = errors.New("hold is no longer active")
	ErrAlreadyCancel = errors.New("booking is already cancelled")
	ErrPNRTaken      = errors.New("pnr already exists") // internal: the service retries with a new PNR
	ErrInvalid       = errors.New("invalid request")
)

// SeatsUnavailableError says WHICH seats could not be held, so the user can
// pick others. Is() makes errors.Is(err, ErrSeatTaken) true for it.
type SeatsUnavailableError struct{ Seats []string }

func (e *SeatsUnavailableError) Error() string {
	return ErrSeatTaken.Error() + ": " + strings.Join(e.Seats, ", ")
}
func (e *SeatsUnavailableError) Is(target error) bool { return target == ErrSeatTaken }

// ValidationError collects every bad field at once (same pattern as Project 1).
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys) // map order is random in Go; sort so the message is stable
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "invalid request: " + strings.Join(parts, "; ")
}
func (e *ValidationError) Is(target error) bool { return target == ErrInvalid }
