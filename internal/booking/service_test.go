package booking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/razi-king/booking-engine/internal/catalog"
	"github.com/razi-king/booking-engine/internal/domain"
	"github.com/razi-king/booking-engine/internal/store/memory"
)

// noTracker ignores scheduling: these tests are about the rules, not timing.
type noTracker struct{}

func (noTracker) Track(string, time.Time) {}
func (noTracker) Forget(string)           {}

var t0 = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

// newTestService wires a service over a seeded memory store with a movable clock.
func newTestService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	st := memory.New()
	if err := st.Seed(context.Background(), catalog.Demo(t0)); err != nil {
		t.Fatal(err)
	}
	now := t0
	svc := NewService(st, noTracker{}, Config{HoldTTL: 10 * time.Minute, MaxSeats: 9, PNRAttempts: 3}, func() time.Time { return now })
	return svc, &now
}

func TestHoldValidation(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.Hold(context.Background(), "", HoldRequest{FlightID: 1, Seats: []string{"40A"}})
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || verr.Fields["user"] == "" || verr.Fields["seats"] == "" {
		t.Fatalf("want user AND seats errors, got %v", err)
	}
	_, err = svc.Hold(context.Background(), "asha", HoldRequest{FlightID: 42, Seats: []string{"1A"}})
	if !errors.As(err, &verr) || verr.Fields["flightId"] == "" {
		t.Fatalf("want flightId error, got %v", err)
	}
}

func TestHoldConfirmFlow(t *testing.T) {
	svc, now := newTestService(t)
	ctx := context.Background()
	h, err := svc.Hold(ctx, "asha", HoldRequest{FlightID: 1, Seats: []string{"12b", "12A"}})
	if err != nil {
		t.Fatal(err)
	}
	if h.Seats[0] != "12A" || !h.ExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("hold = %+v", h)
	}
	// Another user cannot see or confirm Asha's hold: it simply "does not exist" for them.
	if _, err := svc.Confirm(ctx, "ben", h.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stranger confirm: %v", err)
	}
	*now = now.Add(5 * time.Minute)
	b, err := svc.Confirm(ctx, "asha", h.ID)
	if err != nil || len(b.PNR) != 6 {
		t.Fatalf("confirm: %+v %v", b, err)
	}
	m, _ := svc.FlightSeatMap(ctx, 1)
	if len(m.Rows) != 30 || m.Rows[11][0].Status != domain.SeatBooked || m.Available != 178 {
		t.Fatalf("seat map: rows=%d 12A=%s available=%d", len(m.Rows), m.Rows[11][0].Status, m.Available)
	}
	if _, err := svc.Cancel(ctx, "ben", b.PNR); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("only the owner may cancel")
	}
	if _, err := svc.Cancel(ctx, "asha", b.PNR); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredHoldReportsExpired(t *testing.T) {
	svc, now := newTestService(t)
	h, _ := svc.Hold(context.Background(), "asha", HoldRequest{FlightID: 1, Seats: []string{"3C"}})
	*now = now.Add(11 * time.Minute)
	got, _ := svc.GetHold(context.Background(), "asha", h.ID)
	if got.Status != domain.HoldExpired {
		t.Fatalf("status = %s, want expired (lazy expiry)", got.Status)
	}
	if _, err := svc.Confirm(context.Background(), "asha", h.ID); !errors.Is(err, domain.ErrHoldExpired) {
		t.Fatalf("confirm expired: %v", err)
	}
}

func TestConfirmRetriesOnPNRClash(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	// Force a clash: the PNR generator returns "AAAAAA" twice, then "BBBBBB".
	codes := []string{"AAAAAA", "AAAAAA", "BBBBBB"}
	svc.newPNR = func() (string, error) { c := codes[0]; codes = codes[1:]; return c, nil }

	h1, _ := svc.Hold(ctx, "asha", HoldRequest{FlightID: 1, Seats: []string{"1A"}})
	h2, _ := svc.Hold(ctx, "ben", HoldRequest{FlightID: 1, Seats: []string{"1B"}})
	if b, _ := svc.Confirm(ctx, "asha", h1.ID); b.PNR != "AAAAAA" {
		t.Fatalf("first pnr = %s", b.PNR)
	}
	b, err := svc.Confirm(ctx, "ben", h2.ID)
	if err != nil || b.PNR != "BBBBBB" {
		t.Fatalf("after clash: %s %v (should have retried)", b.PNR, err)
	}
}

func TestPNRShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		p, _ := NewPNR()
		for _, c := range p {
			if c == 'O' || c == '0' || c == 'I' || c == '1' {
				t.Fatalf("PNR %s contains a look-alike character", p)
			}
		}
		seen[p] = true
	}
	if len(seen) < 1990 { // 2,000 draws from a billion: duplicates should be essentially absent
		t.Fatalf("only %d unique PNRs out of 2000", len(seen))
	}
}
