package booking

import (
	"container/heap"
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/razi-king/booking-engine/internal/domain"
)

// PROPERTY TEST: after random adds, updates and removes, popping must give
// deadlines in sorted order, and exactly the holds that were not removed.
func TestExpiryHeapProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	base := time.Unix(0, 0)
	for trial := 0; trial < 100; trial++ {
		h := newExpiryHeap()
		want := map[string]time.Time{} // the "oracle": a plain map of what should be inside
		for op := 0; op < 200; op++ {
			id := fmt.Sprintf("h%d", r.IntN(50))
			if r.IntN(4) == 0 {
				h.remove(id)
				delete(want, id)
			} else {
				at := base.Add(time.Duration(r.IntN(1000)) * time.Second)
				h.add(id, at)
				want[id] = at
			}
		}
		// The expected order is simply all remaining deadlines, sorted.
		var exp []time.Time
		for _, at := range want {
			exp = append(exp, at)
		}
		sort.Slice(exp, func(i, j int) bool { return exp[i].Before(exp[j]) })
		// Pop one at a time; each must be the next smallest deadline.
		for i, at := range exp {
			it := heap.Pop(h).(expiryItem)
			if !it.at.Equal(at) || !want[it.holdID].Equal(at) {
				t.Fatalf("trial %d pop %d: got %v want %v", trial, i, it.at, at)
			}
		}
		if h.Len() != 0 || len(h.pos) != 0 {
			t.Fatalf("heap not empty: %d items, %d positions", h.Len(), len(h.pos))
		}
	}
}

// fakeExpirer records sweep times.
type fakeExpirer struct{ calls chan time.Time }

func (f *fakeExpirer) ExpireHolds(_ context.Context, now time.Time) (int, error) {
	f.calls <- now
	return 1, nil
}
func (f *fakeExpirer) ActiveHolds(context.Context) ([]domain.HoldExpiry, error) { return nil, nil }

func TestSchedulerWakesAtDeadline(t *testing.T) {
	f := &fakeExpirer{calls: make(chan time.Time, 10)}
	s := NewScheduler(f, nil, time.Hour) // fallback is an hour: any sweep we see comes from the heap
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	start := time.Now()
	s.Track("a", start.Add(80*time.Millisecond))
	s.Track("b", start.Add(30*time.Millisecond)) // added second but due FIRST: the loop must re-plan
	s.Track("c", start.Add(time.Hour))
	s.Forget("c")

	for i, want := range []time.Duration{30 * time.Millisecond, 80 * time.Millisecond} {
		select {
		case at := <-f.calls:
			if d := at.Sub(start); d < want || d > want+150*time.Millisecond {
				t.Fatalf("sweep %d at %v, want about %v", i, d, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("sweep %d never happened", i)
		}
	}
	select {
	case <-f.calls:
		t.Fatal("a forgotten hold must not cause a sweep")
	case <-time.After(100 * time.Millisecond):
	}
}
