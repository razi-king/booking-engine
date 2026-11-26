// Package memory is a Store that keeps everything in Go maps.
//
// WHEN TO USE IT: local demos and a free deploy with no database, and fast
// unit tests. Data is lost on restart and it cannot be shared between two
// server processes; for that, use store/postgres.
//
// CONCURRENCY DESIGN: ONE mutex guards ALL data (a "coarse-grained" lock).
// Every method locks it for its whole duration, so each method is atomic: no
// other goroutine can see or change data half-way through. That is simple and
// obviously correct, and fast enough in memory (a method takes microseconds).
// Postgres, by contrast, uses fine-grained ROW locks so that bookings on
// different flights never wait for each other. Same contract, different trade-off.
package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/razi-king/booking-engine/internal/catalog"
	"github.com/razi-king/booking-engine/internal/domain"
	"github.com/razi-king/booking-engine/internal/store"
)

// seatRec is a seat plus who currently holds or owns it.
type seatRec struct {
	domain.Seat           // EMBEDDING: seatRec "is a" Seat with extra fields; s.No works directly
	holdID      string    // "" = no hold
	holdUntil   time.Time // meaningful only when holdID != ""
	pnr         string    // "" = not booked
}

// available reports whether the seat can be held at `now`.
// An expired hold does not block the seat (lazy expiry).
func (s *seatRec) available(now time.Time) bool {
	return s.pnr == "" && (s.holdID == "" || !now.Before(s.holdUntil))
}

// status computes what the seat map shows.
func (s *seatRec) status(now time.Time) domain.SeatStatus {
	switch {
	case s.pnr != "":
		return domain.SeatBooked
	case s.available(now):
		return domain.SeatAvailable
	default:
		return domain.SeatHeld
	}
}

// flightRec is a flight with its seats.
type flightRec struct {
	flight domain.Flight
	seats  []*seatRec          // in seat order, for the seat map
	bySeat map[string]*seatRec // seat number → seat, for O(1) lookup by number
}

type idemRec struct {
	fingerprint string
	saved       *store.SavedResponse // nil while the first request is still running
	createdAt   time.Time
}

// Store implements store.Store and store.IdempotencyStore in memory.
type Store struct {
	mu       sync.Mutex
	nextID   int64
	flights  map[int64]*flightRec
	order    []int64 // flight ids in insertion order, so ListFlights is stable
	holds    map[string]*domain.Hold
	bookings map[string]*domain.Booking
	idem     map[string]*idemRec
}

// New returns an empty store. Call Seed to add flights.
func New() *Store {
	return &Store{
		flights:  map[int64]*flightRec{},
		holds:    map[string]*domain.Hold{},
		bookings: map[string]*domain.Booking{},
		idem:     map[string]*idemRec{},
	}
}

// Compile-time checks: the build fails if Store ever stops satisfying the
// interfaces (for example after someone renames a method).
var (
	_ store.Store            = (*Store)(nil)
	_ store.IdempotencyStore = (*Store)(nil)
)

// ── Seeder ──────────────────────────────────────────────────────────────────

func (m *Store) Seed(_ context.Context, flights []catalog.FlightSeed) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.flights) > 0 {
		return nil // already seeded: do nothing (idempotent)
	}
	for _, fs := range flights {
		m.nextID++
		f := fs.Flight
		f.ID = m.nextID
		rec := &flightRec{flight: f, bySeat: make(map[string]*seatRec, len(fs.Seats))}
		for _, s := range fs.Seats {
			sr := &seatRec{Seat: s}
			rec.seats = append(rec.seats, sr)
			rec.bySeat[s.No] = sr
		}
		m.flights[f.ID] = rec
		m.order = append(m.order, f.ID)
	}
	return nil
}

// ── FlightReader ────────────────────────────────────────────────────────────

func (m *Store) ListFlights(_ context.Context) ([]domain.Flight, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.Flight, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.flights[id].flight)
	}
	return out, nil
}

func (m *Store) GetFlight(_ context.Context, id int64) (domain.Flight, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.flights[id]
	if !ok {
		return domain.Flight{}, fmt.Errorf("flight %d: %w", id, domain.ErrNotFound)
	}
	return rec.flight, nil
}

func (m *Store) SeatMap(_ context.Context, flightID int64, now time.Time) ([]domain.Seat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.flights[flightID]
	if !ok {
		return nil, fmt.Errorf("flight %d: %w", flightID, domain.ErrNotFound)
	}
	out := make([]domain.Seat, len(rec.seats))
	for i, s := range rec.seats {
		out[i] = s.Seat               // a COPY of the seat: callers can never modify our data
		out[i].Status = s.status(now) // status is computed, never stored
	}
	return out, nil
}

// ── HoldStore ───────────────────────────────────────────────────────────────

func (m *Store) CreateHold(_ context.Context, h domain.Hold, now time.Time) (domain.Hold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.flights[h.FlightID]
	if !ok {
		return domain.Hold{}, fmt.Errorf("flight %d: %w", h.FlightID, domain.ErrNotFound)
	}

	// PHASE 1: CHECK every seat before changing anything. If we changed seats
	// one by one and the 3rd was taken, we would have to undo the first two.
	// Checking first makes the operation all-or-nothing (atomic).
	var taken []string
	var total int64
	for _, no := range h.Seats {
		s, ok := rec.bySeat[no] // O(1) map lookup instead of scanning 180 seats
		if !ok {
			return domain.Hold{}, fmt.Errorf("seat %s: %w", no, domain.ErrNotFound)
		}
		if !s.available(now) {
			taken = append(taken, no) // keep going: report ALL taken seats, not just the first
			continue
		}
		total += s.PricePaise
	}
	if len(taken) > 0 {
		return domain.Hold{}, &domain.SeatsUnavailableError{Seats: taken}
	}

	// PHASE 2: every seat is free, and nobody else can run while we hold the
	// mutex, so we can now safely claim them all.
	for _, no := range h.Seats {
		s := rec.bySeat[no]
		s.holdID, s.holdUntil = h.ID, h.ExpiresAt
	}
	h.TotalPaise, h.Status, h.CreatedAt = total, domain.HoldActive, now
	h.Seats = append([]string(nil), h.Seats...) // our own copy of the slice
	stored := h
	m.holds[h.ID] = &stored
	return copyHold(stored), nil
}

func (m *Store) GetHold(_ context.Context, id string) (domain.Hold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.holds[id]
	if !ok {
		return domain.Hold{}, fmt.Errorf("hold %s: %w", id, domain.ErrNotFound)
	}
	return copyHold(*h), nil
}

func (m *Store) ConfirmHold(_ context.Context, holdID, pnr string, now time.Time) (domain.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	h, err := m.activeHold(holdID, now)
	if err != nil {
		return domain.Booking{}, err
	}
	if _, exists := m.bookings[pnr]; exists {
		return domain.Booking{}, domain.ErrPNRTaken // the service picks a new PNR and retries
	}
	rec := m.flights[h.FlightID]
	for _, no := range h.Seats {
		s := rec.bySeat[no]
		s.pnr, s.holdID = pnr, "" // the seat moves from "held" to "booked"
	}
	h.Status = domain.HoldConfirmed
	b := domain.Booking{
		PNR: pnr, HoldID: h.ID, FlightID: h.FlightID, UserID: h.UserID,
		Seats: append([]string(nil), h.Seats...), TotalPaise: h.TotalPaise,
		Status: domain.BookingConfirmed, CreatedAt: now,
	}
	m.bookings[pnr] = &b
	return copyBooking(b), nil
}

func (m *Store) ReleaseHold(_ context.Context, holdID string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, err := m.activeHold(holdID, now)
	if err != nil {
		return err
	}
	m.freeSeats(h)
	h.Status = domain.HoldReleased
	return nil
}

func (m *Store) ExpireHolds(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	// O(number of holds). Fine in memory; Postgres uses an index instead.
	for _, h := range m.holds {
		if h.EffectiveStatus(now) == domain.HoldExpired && h.Status == domain.HoldActive {
			m.freeSeats(h)
			h.Status = domain.HoldExpired
			n++
		}
	}
	return n, nil
}

func (m *Store) ActiveHolds(_ context.Context) ([]domain.HoldExpiry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.HoldExpiry
	for _, h := range m.holds {
		if h.Status == domain.HoldActive {
			out = append(out, domain.HoldExpiry{HoldID: h.ID, ExpiresAt: h.ExpiresAt})
		}
	}
	return out, nil
}

// activeHold finds a hold and checks it can still be confirmed or released.
// Shared by ConfirmHold and ReleaseHold (DRY). Caller must hold m.mu.
func (m *Store) activeHold(id string, now time.Time) (*domain.Hold, error) {
	h, ok := m.holds[id]
	if !ok {
		return nil, fmt.Errorf("hold %s: %w", id, domain.ErrNotFound)
	}
	switch h.EffectiveStatus(now) {
	case domain.HoldActive:
		return h, nil
	case domain.HoldExpired:
		return nil, domain.ErrHoldExpired
	default:
		return nil, fmt.Errorf("hold is %s: %w", h.Status, domain.ErrHoldNotActive)
	}
}

// freeSeats clears a hold from its seats, but only seats STILL held by it:
// after an expiry another user may legitimately hold the same seat now.
func (m *Store) freeSeats(h *domain.Hold) {
	rec := m.flights[h.FlightID]
	for _, no := range h.Seats {
		if s := rec.bySeat[no]; s.holdID == h.ID {
			s.holdID = ""
		}
	}
}

// ── BookingStore ────────────────────────────────────────────────────────────

func (m *Store) GetBooking(_ context.Context, pnr string) (domain.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bookings[pnr]
	if !ok {
		return domain.Booking{}, fmt.Errorf("booking %s: %w", pnr, domain.ErrNotFound)
	}
	return copyBooking(*b), nil
}

func (m *Store) CancelBooking(_ context.Context, pnr string, now time.Time) (domain.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bookings[pnr]
	if !ok {
		return domain.Booking{}, fmt.Errorf("booking %s: %w", pnr, domain.ErrNotFound)
	}
	if b.Status == domain.BookingCancelled {
		return domain.Booking{}, domain.ErrAlreadyCancel
	}
	rec := m.flights[b.FlightID]
	for _, no := range b.Seats {
		rec.bySeat[no].pnr = "" // seats go back on sale
	}
	b.Status = domain.BookingCancelled
	t := now
	b.CancelledAt = &t
	return copyBooking(*b), nil
}

// ── IdempotencyStore ────────────────────────────────────────────────────────

func (m *Store) Reserve(_ context.Context, key, fingerprint string, now time.Time) (*store.SavedResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.idem[key]
	switch {
	case !ok:
		m.idem[key] = &idemRec{fingerprint: fingerprint, createdAt: now}
		return nil, nil // first time: caller does the work
	case rec.fingerprint != fingerprint:
		return nil, store.ErrKeyMismatch
	case rec.saved == nil:
		return nil, store.ErrKeyInProgress
	default:
		cp := *rec.saved
		return &cp, nil // replay
	}
}

func (m *Store) Complete(_ context.Context, key string, resp store.SavedResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.idem[key]; ok {
		resp.Body = append([]byte(nil), resp.Body...)
		rec.saved = &resp
	}
	return nil
}

func (m *Store) Forget(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.idem, key)
	return nil
}

func (m *Store) PurgeKeys(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, rec := range m.idem {
		if rec.createdAt.Before(before) {
			delete(m.idem, k) // deleting while ranging over a map is allowed in Go
			n++
		}
	}
	return n, nil
}

// ── copies ──────────────────────────────────────────────────────────────────

// copyHold / copyBooking return values whose slices do not share memory with
// ours. Without this, a caller doing hold.Seats[0] = "1A" would silently edit
// the store's data without taking the lock: a data race.
func copyHold(h domain.Hold) domain.Hold {
	h.Seats = append([]string(nil), h.Seats...)
	return h
}

func copyBooking(b domain.Booking) domain.Booking {
	b.Seats = append([]string(nil), b.Seats...)
	return b
}
