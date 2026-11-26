package postgres

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/razi-king/booking-engine/internal/catalog"
	"github.com/razi-king/booking-engine/internal/store/storetest"
)

// TestNaiveCheckThenWriteDoubleBooks is a TEACHING test. It runs the broken
// "check, then write" approach WITHOUT FOR UPDATE and shows that several users
// are told they won the same seat. pg_sleep widens the gap between the check
// and the write, so the race shows up every time instead of once in a while.
//
// Compare with the contract test "last seat: exactly one winner", which runs
// the real CreateHold (with FOR UPDATE) and always gets exactly one winner.
func TestNaiveCheckThenWriteDoubleBooks(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Reset(ctx)
	s.Seed(ctx, catalog.Demo(storetest.T0))

	naiveHold := func(user string) bool {
		var won bool
		s.inTx(ctx, func(tx *sql.Tx) error {
			var free bool
			// CHECK (no lock!)
			tx.QueryRowContext(ctx, `SELECT booking_pnr IS NULL FROM seats WHERE flight_id = 1 AND seat_no = '15A'`).Scan(&free)
			if !free {
				return nil
			}
			tx.ExecContext(ctx, `SELECT pg_sleep(0.05)`) // the dangerous gap, made wider
			// WRITE: "only if still free" would fix it; the naive code just writes.
			tx.ExecContext(ctx, `UPDATE seats SET booking_pnr = NULL WHERE flight_id = 1 AND seat_no = '15A'`)
			won = true
			return nil
		})
		return won
	}

	var winners atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if naiveHold("user") {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	t.Logf("naive check-then-write: %d of 10 users were told they got seat 15A", winners.Load())
	if winners.Load() < 2 {
		t.Fatal("expected the naive approach to double-book")
	}
}
