// Package booking holds the business rules of booking: how long holds last,
// how many seats per hold, who may confirm what, and how PNRs are made.
//
// It does not know about HTTP or SQL. It talks to storage only through the
// store.Store interface, so it works unchanged with Postgres or memory.
package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/razi-king/booking-engine/internal/domain"
	"github.com/razi-king/booking-engine/internal/store"
)

// Config holds the business knobs.
type Config struct {
	HoldTTL     time.Duration // how long a hold lasts (10 minutes by default)
	MaxSeats    int           // seats per hold (9, like most airlines)
	PNRAttempts int           // retries if a random PNR already exists
}

// Tracker is what the service needs from the scheduler. An interface, so
// tests can pass a no-op and the service does not depend on the real loop.
type Tracker interface {
	Track(holdID string, at time.Time)
	Forget(holdID string)
}

// Service implements the booking use cases.
type Service struct {
	store   store.Store
	tracker Tracker
	cfg     Config
	now     func() time.Time
	newID   func() (string, error)
	newPNR  func() (string, error)
}

// NewService wires a service. now may be nil (time.Now).
// Ids and PNRs come from functions so tests can make them predictable.
func NewService(s store.Store, t Tracker, cfg Config, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() } // UTC: one unambiguous time zone in every response
	}
	return &Service{store: s, tracker: t, cfg: cfg, now: now, newID: NewID, newPNR: NewPNR}
}

// HoldRequest is what a user asks for.
type HoldRequest struct {
	FlightID int64    `json:"flightId"`
	Seats    []string `json:"seats"`
}

// SeatMap is a flight with its seats grouped into rows for drawing.
type SeatMap struct {
	Flight    domain.Flight   `json:"flight"`
	Letters   string          `json:"letters"`   // "ABCDEF"
	Rows      [][]domain.Seat `json:"rows"`      // Rows[0] = row 1, each with 6 seats
	Available int             `json:"available"` // free seats right now
}

// FlightSeatMap returns a flight's seats as a 2-D grid: rows × letters.
//
// DSA: the store returns a flat, sorted list of 180 seats. Because it is
// sorted and every row has exactly 6 seats, seat k belongs to row k/6. One
// pass builds the grid: O(n), no searching.
func (s *Service) FlightSeatMap(ctx context.Context, flightID int64) (SeatMap, error) {
	f, err := s.store.GetFlight(ctx, flightID)
	if err != nil {
		return SeatMap{}, err
	}
	seats, err := s.store.SeatMap(ctx, flightID, s.now())
	if err != nil {
		return SeatMap{}, err
	}
	perRow := len(domain.SeatLetters)
	m := SeatMap{Flight: f, Letters: domain.SeatLetters, Rows: make([][]domain.Seat, 0, f.Rows)}
	for i := 0; i < len(seats); i += perRow {
		end := min(i+perRow, len(seats)) // min is built in since Go 1.21
		m.Rows = append(m.Rows, seats[i:end])
		for _, st := range seats[i:end] {
			if st.Status == domain.SeatAvailable {
				m.Available++
			}
		}
	}
	return m, nil
}

// ListFlights returns every flight.
func (s *Service) ListFlights(ctx context.Context) ([]domain.Flight, error) {
	return s.store.ListFlights(ctx)
}

// Hold validates a request and reserves the seats for HoldTTL.
func (s *Service) Hold(ctx context.Context, userID string, req HoldRequest) (domain.Hold, error) {
	// Validate everything first and collect ALL problems (one round trip for the user).
	fields := map[string]string{}
	if userID == "" {
		fields["user"] = "X-User-ID header is required"
	}
	f, err := s.store.GetFlight(ctx, req.FlightID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Hold{}, err // a real database problem, not a user mistake
		}
		fields["flightId"] = "no such flight"
	}
	var seats []string
	if f.ID != 0 {
		if seats, err = domain.NormalizeSeats(req.Seats, f.Rows, s.cfg.MaxSeats); err != nil {
			fields["seats"] = err.Error()
		}
	}
	if len(fields) > 0 {
		return domain.Hold{}, &domain.ValidationError{Fields: fields}
	}

	id, err := s.newID()
	if err != nil {
		return domain.Hold{}, err
	}
	now := s.now()
	h, err := s.store.CreateHold(ctx, domain.Hold{
		ID: id, FlightID: f.ID, UserID: userID, Seats: seats, ExpiresAt: now.Add(s.cfg.HoldTTL),
	}, now)
	if err != nil {
		return domain.Hold{}, err
	}
	s.tracker.Track(h.ID, h.ExpiresAt) // the scheduler will expire it on time
	return h, nil
}

// GetHold returns a hold, but only to its owner, with its effective status.
func (s *Service) GetHold(ctx context.Context, userID, holdID string) (domain.Hold, error) {
	h, err := s.ownHold(ctx, userID, holdID)
	if err != nil {
		return domain.Hold{}, err
	}
	h.Status = h.EffectiveStatus(s.now()) // report "expired" even before the sweeper runs
	return h, nil
}

// Confirm turns a hold into a booking. In a real system this runs after the
// payment provider says the payment succeeded.
func (s *Service) Confirm(ctx context.Context, userID, holdID string) (domain.Booking, error) {
	if _, err := s.ownHold(ctx, userID, holdID); err != nil {
		return domain.Booking{}, err
	}
	// RETRY LOOP for the (rare) case of a random PNR that already exists.
	// The store's unique key detects the clash; we just roll the dice again.
	for attempt := 1; attempt <= s.cfg.PNRAttempts; attempt++ {
		pnr, err := s.newPNR()
		if err != nil {
			return domain.Booking{}, err
		}
		b, err := s.store.ConfirmHold(ctx, holdID, pnr, s.now())
		if errors.Is(err, domain.ErrPNRTaken) {
			continue // try another PNR
		}
		if err != nil {
			return domain.Booking{}, err
		}
		s.tracker.Forget(holdID) // confirmed: nothing to expire any more
		return b, nil
	}
	return domain.Booking{}, fmt.Errorf("could not find a free PNR after %d attempts", s.cfg.PNRAttempts)
}

// Release frees a hold early ("I changed my mind").
func (s *Service) Release(ctx context.Context, userID, holdID string) error {
	if _, err := s.ownHold(ctx, userID, holdID); err != nil {
		return err
	}
	if err := s.store.ReleaseHold(ctx, holdID, s.now()); err != nil {
		return err
	}
	s.tracker.Forget(holdID)
	return nil
}

// GetBooking returns a booking to its owner.
func (s *Service) GetBooking(ctx context.Context, userID, pnr string) (domain.Booking, error) {
	return s.ownBooking(ctx, userID, pnr)
}

// Cancel cancels a booking and puts its seats back on sale.
func (s *Service) Cancel(ctx context.Context, userID, pnr string) (domain.Booking, error) {
	if _, err := s.ownBooking(ctx, userID, pnr); err != nil {
		return domain.Booking{}, err
	}
	return s.store.CancelBooking(ctx, pnr, s.now())
}

// ownHold loads a hold and checks the caller owns it. Shared by four methods (DRY).
//
// SECURITY: a stranger's hold answers "not found", not "forbidden". Saying
// "forbidden" would confirm that the id exists, which helps attackers guess.
func (s *Service) ownHold(ctx context.Context, userID, holdID string) (domain.Hold, error) {
	h, err := s.store.GetHold(ctx, holdID)
	if err != nil {
		return domain.Hold{}, err
	}
	if h.UserID != userID {
		return domain.Hold{}, fmt.Errorf("hold %s: %w", holdID, domain.ErrNotFound)
	}
	return h, nil
}

// ownBooking is ownHold for bookings.
func (s *Service) ownBooking(ctx context.Context, userID, pnr string) (domain.Booking, error) {
	b, err := s.store.GetBooking(ctx, pnr)
	if err != nil {
		return domain.Booking{}, err
	}
	if b.UserID != userID {
		return domain.Booking{}, fmt.Errorf("booking %s: %w", pnr, domain.ErrNotFound)
	}
	return b, nil
}
