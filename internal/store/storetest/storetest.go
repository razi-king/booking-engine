// Package storetest is ONE test suite that every Store implementation must pass.
//
// WHY? The booking service is written against the store.Store interface. The
// Liskov Substitution Principle says any implementation must be usable in its
// place without surprises. These "contract tests" turn that promise into code:
// memory_test.go and postgres_test.go each call Run with their own store, so
// the two can never quietly behave differently. DRY too: the tests are
// written once instead of twice.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/razi-king/booking-engine/internal/catalog"
	"github.com/razi-king/booking-engine/internal/domain"
	"github.com/razi-king/booking-engine/internal/store"
)

// Full is what an implementation must provide to be tested.
type Full interface {
	store.Store
	store.IdempotencyStore
}

// Factory returns a fresh, EMPTY store for each sub-test.
type Factory func(t *testing.T) Full

// T0 is a fixed "now". Every test moves time explicitly, so nothing depends
// on the real clock or on how fast the machine is.
var T0 = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

// Run executes every contract test against stores made by newStore.
func Run(t *testing.T, newStore Factory) {
	tests := map[string]func(*testing.T, Full){
		"seed and seat map":              testSeedAndSeatMap,
		"hold is all-or-nothing":         testHoldAllOrNothing,
		"expired hold frees the seat":    testLazyExpiry,
		"confirm then cancel":            testConfirmCancel,
		"confirm rules":                  testConfirmRules,
		"release":                        testRelease,
		"sweeper expires holds":          testExpireHolds,
		"last seat: exactly one winner":  testLastSeatRace,
		"overlapping holds: no deadlock": testNoDeadlock,
		"idempotency keys":               testIdempotency,
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) { fn(t, seeded(t, newStore)) })
	}
}

// seeded returns a store with the 3 demo flights (DRY setup for every test).
func seeded(t *testing.T, newStore Factory) Full {
	t.Helper()
	s := newStore(t)
	if err := s.Seed(context.Background(), catalog.Demo(T0)); err != nil {
		t.Fatal(err)
	}
	// Seeding twice must be harmless.
	if err := s.Seed(context.Background(), catalog.Demo(T0)); err != nil {
		t.Fatal(err)
	}
	return s
}

// hold is a tiny builder for test holds.
func hold(id, user string, ttl time.Duration, seats ...string) domain.Hold {
	return domain.Hold{ID: id, FlightID: 1, UserID: user, Seats: seats, ExpiresAt: T0.Add(ttl)}
}

// statusOf finds one seat's status on the seat map.
func statusOf(t *testing.T, s Full, no string, now time.Time) domain.SeatStatus {
	t.Helper()
	seats, err := s.SeatMap(context.Background(), 1, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range seats {
		if st.No == no {
			return st.Status
		}
	}
	t.Fatalf("seat %s not on map", no)
	return ""
}

func testSeedAndSeatMap(t *testing.T, s Full) {
	ctx := context.Background()
	flights, _ := s.ListFlights(ctx)
	if len(flights) != 3 || flights[0].Number != "6E-2031" {
		t.Fatalf("flights = %+v", flights)
	}
	seats, err := s.SeatMap(ctx, 1, T0)
	if err != nil || len(seats) != 180 {
		t.Fatalf("seats = %d, err %v", len(seats), err)
	}
	// Seat order: row NUMBER order, so seat index 6 is 2A and 54 is 10A (not "10A" after "1F").
	if seats[0].No != "1A" || seats[6].No != "2A" || seats[54].No != "10A" {
		t.Fatalf("bad order: %s %s %s", seats[0].No, seats[6].No, seats[54].No)
	}
	if _, err := s.SeatMap(ctx, 99, T0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown flight: %v", err)
	}
}

func testHoldAllOrNothing(t *testing.T, s Full) {
	ctx := context.Background()
	h, err := s.CreateHold(ctx, hold("h1", "asha", 10*time.Minute, "12A", "12B"), T0)
	if err != nil {
		t.Fatal(err)
	}
	if h.TotalPaise != domain.SeatPrice(550000, 12, "A")+domain.SeatPrice(550000, 12, "B") {
		t.Fatalf("total = %d", h.TotalPaise)
	}
	// Ben wants 12B and 12C. 12B is taken → he gets NEITHER, and is told which one.
	_, err = s.CreateHold(ctx, hold("h2", "ben", 10*time.Minute, "12B", "12C"), T0)
	var sue *domain.SeatsUnavailableError
	if !errors.As(err, &sue) || len(sue.Seats) != 1 || sue.Seats[0] != "12B" {
		t.Fatalf("want 12B unavailable, got %v", err)
	}
	if got := statusOf(t, s, "12C", T0); got != domain.SeatAvailable {
		t.Fatalf("12C = %s: a failed hold must not keep any seat", got)
	}
	if _, err := s.CreateHold(ctx, hold("h3", "ben", time.Minute, "99Z"), T0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown seat: %v", err)
	}
}

func testLazyExpiry(t *testing.T, s Full) {
	ctx := context.Background()
	if _, err := s.CreateHold(ctx, hold("h1", "asha", 10*time.Minute, "5A"), T0); err != nil {
		t.Fatal(err)
	}
	later := T0.Add(10 * time.Minute) // exactly at the deadline
	// No sweeper has run, yet the seat already shows as available...
	if got := statusOf(t, s, "5A", later); got != domain.SeatAvailable {
		t.Fatalf("5A = %s at the deadline, want available", got)
	}
	// ...and someone else can hold it.
	if _, err := s.CreateHold(ctx, hold("h2", "ben", 20*time.Minute, "5A"), later); err != nil {
		t.Fatalf("seat with an expired hold must be holdable: %v", err)
	}
	// Asha's old hold can no longer be confirmed.
	if _, err := s.ConfirmHold(ctx, "h1", "AAAAAA", later); !errors.Is(err, domain.ErrHoldExpired) {
		t.Fatalf("confirm expired: %v", err)
	}
	// And sweeping Asha's hold must NOT free Ben's.
	if _, err := s.ExpireHolds(ctx, later); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, s, "5A", later); got != domain.SeatHeld {
		t.Fatalf("5A = %s: sweeping an old hold freed the new one", got)
	}
}

func testConfirmCancel(t *testing.T, s Full) {
	ctx := context.Background()
	s.CreateHold(ctx, hold("h1", "asha", 10*time.Minute, "7C", "7D"), T0)
	b, err := s.ConfirmHold(ctx, "h1", "K7QX2M", T0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != domain.BookingConfirmed || len(b.Seats) != 2 || b.TotalPaise == 0 {
		t.Fatalf("booking = %+v", b)
	}
	if got := statusOf(t, s, "7C", T0.Add(time.Hour)); got != domain.SeatBooked {
		t.Fatalf("7C = %s, want booked (and it never expires)", got)
	}
	got, _ := s.GetBooking(ctx, "K7QX2M")
	if got.HoldID != "h1" || got.Seats[1] != "7D" {
		t.Fatalf("get booking = %+v", got)
	}
	if h, _ := s.GetHold(ctx, "h1"); h.Status != domain.HoldConfirmed {
		t.Fatalf("hold status = %s", h.Status)
	}
	c, err := s.CancelBooking(ctx, "K7QX2M", T0.Add(2*time.Hour))
	if err != nil || c.Status != domain.BookingCancelled || c.CancelledAt == nil {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if got := statusOf(t, s, "7C", T0.Add(2*time.Hour)); got != domain.SeatAvailable {
		t.Fatal("cancelled seats must go back on sale")
	}
	if _, err := s.CancelBooking(ctx, "K7QX2M", T0.Add(3*time.Hour)); !errors.Is(err, domain.ErrAlreadyCancel) {
		t.Fatalf("double cancel: %v", err)
	}
}

func testConfirmRules(t *testing.T, s Full) {
	ctx := context.Background()
	s.CreateHold(ctx, hold("h1", "asha", 10*time.Minute, "8A"), T0)
	s.CreateHold(ctx, hold("h2", "ben", 10*time.Minute, "8B"), T0)
	if _, err := s.ConfirmHold(ctx, "h1", "PNR001", T0); err != nil {
		t.Fatal(err)
	}
	// A hold can be confirmed only once.
	if _, err := s.ConfirmHold(ctx, "h1", "PNR002", T0); !errors.Is(err, domain.ErrHoldNotActive) {
		t.Fatalf("second confirm: %v", err)
	}
	// A PNR clash is reported so the service can retry with another code, and
	// nothing changes: Ben's hold is still active afterwards.
	if _, err := s.ConfirmHold(ctx, "h2", "PNR001", T0); !errors.Is(err, domain.ErrPNRTaken) {
		t.Fatalf("duplicate pnr: %v", err)
	}
	if _, err := s.ConfirmHold(ctx, "h2", "PNR003", T0); err != nil {
		t.Fatalf("retry after pnr clash: %v", err)
	}
	if _, err := s.ConfirmHold(ctx, "nope", "PNR004", T0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown hold: %v", err)
	}
}

func testRelease(t *testing.T, s Full) {
	ctx := context.Background()
	s.CreateHold(ctx, hold("h1", "asha", 10*time.Minute, "9F"), T0)
	if err := s.ReleaseHold(ctx, "h1", T0); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, s, "9F", T0); got != domain.SeatAvailable {
		t.Fatalf("9F = %s after release", got)
	}
	if err := s.ReleaseHold(ctx, "h1", T0); !errors.Is(err, domain.ErrHoldNotActive) {
		t.Fatalf("double release: %v", err)
	}
}

func testExpireHolds(t *testing.T, s Full) {
	ctx := context.Background()
	s.CreateHold(ctx, hold("short", "a", time.Minute, "10A"), T0)
	s.CreateHold(ctx, hold("long", "b", time.Hour, "10B"), T0)
	active, _ := s.ActiveHolds(ctx)
	if len(active) != 2 {
		t.Fatalf("active = %d", len(active))
	}
	n, err := s.ExpireHolds(ctx, T0.Add(2*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("expired %d, err %v; want 1", n, err)
	}
	if h, _ := s.GetHold(ctx, "short"); h.Status != domain.HoldExpired {
		t.Fatalf("short = %s", h.Status)
	}
	if n, _ := s.ExpireHolds(ctx, T0.Add(2*time.Minute)); n != 0 {
		t.Fatal("expiring twice must do nothing the second time")
	}
}

// testLastSeatRace is THE test of this project: many users try to hold the
// same seat at the same instant; exactly one may win.
func testLastSeatRace(t *testing.T, s Full) {
	const users = 50
	var wins, losses atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{}) // closed to release all goroutines at once
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // every goroutine waits here, then they all go together
			_, err := s.CreateHold(context.Background(), hold(fmt.Sprintf("h%d", i), fmt.Sprintf("u%d", i), time.Minute, "15A"), T0)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, domain.ErrSeatTaken):
				losses.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != users-1 {
		t.Fatalf("wins = %d, losses = %d: the seat was double-held!", wins.Load(), losses.Load())
	}
}

// testNoDeadlock: pairs of users hold overlapping seat pairs at once, e.g.
// {20A,20B} and {20B,20C}. Without a fixed lock order Postgres would report
// "deadlock detected" for some of them. Every outcome must be success or
// ErrSeatTaken, nothing else.
func testNoDeadlock(t *testing.T, s Full) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	letters := []string{"A", "B", "C", "D", "E", "F"}
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			row := 20 + i%3
			a, b := letters[i%5], letters[i%5+1] // neighbouring seats: pairs overlap
			seats := []string{domain.SeatNo(row, a), domain.SeatNo(row, b)}
			domain.SortSeatNos(seats)
			_, err := s.CreateHold(context.Background(), hold(fmt.Sprintf("d%d", i), "u", time.Minute, seats...), T0)
			if err != nil && !errors.Is(err, domain.ErrSeatTaken) {
				t.Errorf("unexpected error (deadlock?): %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
}

func testIdempotency(t *testing.T, s Full) {
	ctx := context.Background()
	saved, err := s.Reserve(ctx, "k1", "fp-A", T0)
	if err != nil || saved != nil {
		t.Fatalf("first reserve: %v %v", saved, err)
	}
	if _, err := s.Reserve(ctx, "k1", "fp-A", T0); !errors.Is(err, store.ErrKeyInProgress) {
		t.Fatalf("while running: %v", err)
	}
	if _, err := s.Reserve(ctx, "k1", "fp-B", T0); !errors.Is(err, store.ErrKeyMismatch) {
		t.Fatalf("different body: %v", err)
	}
	if err := s.Complete(ctx, "k1", store.SavedResponse{Status: 201, Body: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	saved, err = s.Reserve(ctx, "k1", "fp-A", T0)
	if err != nil || saved == nil || saved.Status != 201 || string(saved.Body) != `{"ok":true}` {
		t.Fatalf("replay: %+v %v", saved, err)
	}
	// Forget allows a fresh attempt (used after a 5xx).
	s.Reserve(ctx, "k2", "fp", T0)
	s.Forget(ctx, "k2")
	if saved, err := s.Reserve(ctx, "k2", "fp", T0); err != nil || saved != nil {
		t.Fatalf("after forget: %v %v", saved, err)
	}
	if n, _ := s.PurgeKeys(ctx, T0.Add(time.Second)); n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
}
