// Package store defines WHAT the booking service needs from storage, without
// saying HOW it is stored. Two packages implement it: store/memory and
// store/postgres.
//
// INTERFACE SEGREGATION: instead of one giant interface, there are small ones
// grouped by job. The HTTP seat-map handler only needs FlightReader; the
// idempotency middleware only needs IdempotencyStore. Each consumer asks for
// the least it needs, so fakes in tests stay tiny.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/razi-king/booking-engine/internal/catalog"
	"github.com/razi-king/booking-engine/internal/domain"
)

// FlightReader reads flights and seat maps.
type FlightReader interface {
	ListFlights(ctx context.Context) ([]domain.Flight, error)
	GetFlight(ctx context.Context, id int64) (domain.Flight, error)
	// SeatMap returns every seat in seat order, with Status computed at `now`.
	SeatMap(ctx context.Context, flightID int64, now time.Time) ([]domain.Seat, error)
}

// HoldStore creates and changes holds. Every method is ATOMIC: it either does
// all of its work or none, even with many concurrent callers.
type HoldStore interface {
	// CreateHold reserves h.Seats (already normalised and sorted) for h.UserID
	// if EVERY seat is available at `now`. It fills in TotalPaise and returns
	// the stored hold. If any seat is taken it returns *SeatsUnavailableError
	// and holds nothing.
	CreateHold(ctx context.Context, h domain.Hold, now time.Time) (domain.Hold, error)
	GetHold(ctx context.Context, id string) (domain.Hold, error)
	// ConfirmHold turns an active, unexpired hold into a booking with the given PNR.
	ConfirmHold(ctx context.Context, holdID, pnr string, now time.Time) (domain.Booking, error)
	// ReleaseHold frees an active hold's seats early.
	ReleaseHold(ctx context.Context, holdID string, now time.Time) error
	// ExpireHolds marks every active hold whose deadline has passed as expired
	// and frees its seats. Returns how many holds it expired.
	ExpireHolds(ctx context.Context, now time.Time) (int, error)
	// ActiveHolds lists active holds' deadlines (used to fill the scheduler at start-up).
	ActiveHolds(ctx context.Context) ([]domain.HoldExpiry, error)
}

// BookingStore reads and cancels bookings.
type BookingStore interface {
	GetBooking(ctx context.Context, pnr string) (domain.Booking, error)
	CancelBooking(ctx context.Context, pnr string, now time.Time) (domain.Booking, error)
}

// Seeder loads demo data into an empty store.
type Seeder interface {
	// Seed inserts the flights if the store has none. It is safe to call on
	// every start-up (idempotent): a store that already has flights is left alone.
	Seed(ctx context.Context, flights []catalog.FlightSeed) error
}

// Store is everything the booking service uses: interface EMBEDDING composes
// the small interfaces into a bigger one without repeating any method.
type Store interface {
	FlightReader
	HoldStore
	BookingStore
	Seeder
}

// ── Idempotency ─────────────────────────────────────────────────────────────

// SavedResponse is a response we already sent once for an idempotency key.
type SavedResponse struct {
	Status int
	Body   []byte
}

var (
	// ErrKeyInProgress: the first request with this key is still running.
	ErrKeyInProgress = errors.New("a request with this idempotency key is still in progress")
	// ErrKeyMismatch: the key was used before with a DIFFERENT request body.
	ErrKeyMismatch = errors.New("idempotency key was already used for a different request")
)

// IdempotencyStore remembers responses by key.
type IdempotencyStore interface {
	// Reserve claims `key` for a request whose content hashes to `fingerprint`.
	//   - first time:            returns (nil, nil): go ahead and do the work
	//   - same request, finished: returns the saved response to replay
	//   - same request, running:  ErrKeyInProgress
	//   - different request:      ErrKeyMismatch
	Reserve(ctx context.Context, key, fingerprint string, now time.Time) (*SavedResponse, error)
	// Complete stores the response for a reserved key.
	Complete(ctx context.Context, key string, resp SavedResponse) error
	// Forget removes a reservation (after a server error, so the client may retry).
	Forget(ctx context.Context, key string) error
	// PurgeKeys deletes keys created before `before` (keys are kept for 24 h).
	PurgeKeys(ctx context.Context, before time.Time) (int, error)
}
