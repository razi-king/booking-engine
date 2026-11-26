package booking

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/razi-king/booking-engine/internal/domain"
)

// Expirer is the one store method the scheduler needs (Interface Segregation).
type Expirer interface {
	ExpireHolds(ctx context.Context, now time.Time) (int, error)
	ActiveHolds(ctx context.Context) ([]domain.HoldExpiry, error)
}

// Scheduler wakes up EXACTLY when the next hold expires and asks the store to
// expire it.
//
// WHY NOT JUST POLL EVERY SECOND? Polling runs a database query 86,400 times
// a day even when nothing expires, and is still up to 1 s late. The scheduler
// sleeps until the earliest deadline (from the min-heap), so it does work only
// when there is work. It ALSO sweeps every `fallback` interval, because holds
// created by OTHER server instances are not in this instance's heap.
//
// Remember: expiry is already correct without the scheduler (lazy expiry, see
// domain.Hold.EffectiveStatus). The scheduler keeps the data tidy: hold
// statuses up to date and seats unlinked from dead holds.
type Scheduler struct {
	store    Expirer
	now      func() time.Time
	fallback time.Duration

	mu   sync.Mutex
	heap *expiryHeap
	wake chan struct{} // a signal: "the earliest deadline may have changed"

	// Sweeps counts store sweeps (read by tests and /metrics).
	sweeps  int
	expired int
}

// NewScheduler builds a scheduler. now may be nil (uses time.Now).
func NewScheduler(s Expirer, now func() time.Time, fallback time.Duration) *Scheduler {
	if now == nil {
		now = time.Now
	}
	return &Scheduler{store: s, now: now, fallback: fallback, heap: newExpiryHeap(),
		wake: make(chan struct{}, 1)} // capacity 1: a pending wake-up is never lost, extra ones are dropped
}

// Track adds or updates a hold's deadline.
func (s *Scheduler) Track(holdID string, at time.Time) {
	s.mu.Lock()
	s.heap.add(holdID, at)
	s.mu.Unlock()
	s.poke()
}

// Forget stops tracking a hold (it was confirmed or released).
func (s *Scheduler) Forget(holdID string) {
	s.mu.Lock()
	s.heap.remove(holdID)
	s.mu.Unlock()
	s.poke()
}

// poke wakes the loop WITHOUT blocking: if a wake-up is already pending, the
// default branch drops this one. One pending signal is enough to make the
// loop re-read the heap.
func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Stats returns how many sweeps ran and how many holds they expired.
func (s *Scheduler) Stats() (sweeps, expired int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweeps, s.expired
}

// Run loads existing active holds, then loops until ctx is cancelled.
// Call it in its own goroutine: go scheduler.Run(ctx).
func (s *Scheduler) Run(ctx context.Context) {
	// After a restart the heap is empty, but the database may still have
	// active holds. Load them so they expire on time.
	if active, err := s.store.ActiveHolds(ctx); err != nil {
		slog.Error("load active holds", "err", err)
	} else {
		for _, h := range active {
			s.Track(h.HoldID, h.ExpiresAt)
		}
	}

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		// How long until the earliest deadline? Never longer than `fallback`.
		wait := s.fallback
		s.mu.Lock()
		if at, ok := s.heap.next(); ok {
			if d := at.Sub(s.now()); d < wait {
				wait = d
			}
		}
		s.mu.Unlock()
		if wait < 0 {
			wait = 0
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return // graceful shutdown
		case <-s.wake:
			// A hold was added or removed: stop the timer and recompute the wait.
			// Draining timer.C after a failed Stop avoids a stale tick later.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			s.sweep(ctx)
		}
	}
}

// sweep expires every overdue hold in ONE store call, however many are due.
func (s *Scheduler) sweep(ctx context.Context) {
	now := s.now()
	n, err := s.store.ExpireHolds(ctx, now)
	s.mu.Lock()
	s.heap.popDue(now)
	s.sweeps++
	s.expired += n
	s.mu.Unlock()
	if err != nil {
		slog.Error("expire holds", "err", err)
		return
	}
	if n > 0 {
		slog.Info("holds expired", "count", n)
	}
}
