// Package postgres is the production Store, backed by PostgreSQL.
//
// The key idea: every operation that must be atomic runs inside a database
// TRANSACTION, and rows that will be changed are locked first with
// SELECT ... FOR UPDATE, always in the same order. See the docs, section 2.
package postgres

import (
	"context"
	"database/sql"
	_ "embed" // enables the //go:embed directive below
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq" // Postgres driver; also gives pq.Array for TEXT[] columns

	"github.com/razi-king/booking-engine/internal/catalog"
	"github.com/razi-king/booking-engine/internal/domain"
	"github.com/razi-king/booking-engine/internal/store"
)

// schema is the content of schema.sql, compiled INTO the binary by go:embed.
// The Docker image therefore needs no extra files to create its tables.
//
//go:embed schema.sql
var schema string

// Store implements store.Store and store.IdempotencyStore on PostgreSQL.
type Store struct{ db *sql.DB }

var (
	_ store.Store            = (*Store)(nil)
	_ store.IdempotencyStore = (*Store)(nil)
)

// Open connects, checks the connection and creates the tables.
//
// sql.Open does not connect yet; it only prepares a CONNECTION POOL. The pool
// opens connections on demand and reuses them, because opening a Postgres
// connection costs a few milliseconds and some memory on the server.
func Open(ctx context.Context, url string) (*Store, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)                  // never more than 20 at once: protects the database
	db.SetMaxIdleConns(10)                  // keep 10 warm for the next requests
	db.SetConnMaxLifetime(30 * time.Minute) // recycle old ones (helps behind load balancers)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the pool.
func (s *Store) Close() error { return s.db.Close() }

// Ping is used by /healthz.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// inTx runs fn inside a transaction: COMMIT if fn returns nil, ROLLBACK otherwise.
//
// DRY: every write method needs begin/commit/rollback. Writing it once here
// means no method can forget the rollback on an error path. The deferred
// Rollback is a no-op after a successful Commit.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil) // nil options = READ COMMITTED, Postgres's default
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // error after Commit is expected and meaningless
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// isUniqueViolation reports whether err is Postgres error 23505 (duplicate key).
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// notFound turns sql.ErrNoRows into our domain error, leaving others alone.
func notFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, domain.ErrNotFound)
	}
	return err
}

// ── Seeder ──────────────────────────────────────────────────────────────────

func (s *Store) Seed(ctx context.Context, flights []catalog.FlightSeed) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// Lock the whole flights table for this transaction, so two servers
		// starting at the same moment cannot BOTH see "empty" and both seed.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE flights IN EXCLUSIVE MODE`); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM flights`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil // already seeded
		}
		// A PREPARED STATEMENT is parsed once and executed 540 times (3 × 180 seats),
		// instead of sending and parsing the same SQL text 540 times.
		seatStmt, err := tx.PrepareContext(ctx, `INSERT INTO seats
			(flight_id, seat_no, seat_row, seat_letter, cabin, price_paise) VALUES ($1,$2,$3,$4,$5,$6)`)
		if err != nil {
			return err
		}
		defer seatStmt.Close()
		for _, fs := range flights {
			f := fs.Flight
			var id int64
			// $1, $2 ... are PLACEHOLDERS. Values travel separately from the SQL
			// text, so user input can never be executed as SQL (no SQL injection).
			if err := tx.QueryRowContext(ctx, `INSERT INTO flights (number, origin, destination, departure, seat_rows)
				VALUES ($1,$2,$3,$4,$5) RETURNING id`, f.Number, f.From, f.To, f.Departure, f.Rows).Scan(&id); err != nil {
				return err
			}
			for _, st := range fs.Seats {
				if _, err := seatStmt.ExecContext(ctx, id, st.No, st.Row, st.Letter, string(st.Cabin), st.PricePaise); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ── FlightReader ────────────────────────────────────────────────────────────

const flightCols = `id, number, origin, destination, departure, seat_rows`

// scanFlight reads one flight row. Shared by ListFlights and GetFlight (DRY).
// The interface{ Scan } parameter accepts both *sql.Row and *sql.Rows.
func scanFlight(r interface{ Scan(...any) error }) (domain.Flight, error) {
	var f domain.Flight
	err := r.Scan(&f.ID, &f.Number, &f.From, &f.To, &f.Departure, &f.Rows)
	f.Departure = f.Departure.UTC()
	return f, err
}

func (s *Store) ListFlights(ctx context.Context) ([]domain.Flight, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+flightCols+` FROM flights ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // ALWAYS close rows, or the connection never returns to the pool
	var out []domain.Flight
	for rows.Next() {
		f, err := scanFlight(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err() // rows.Err reports an error that ended the loop early
}

func (s *Store) GetFlight(ctx context.Context, id int64) (domain.Flight, error) {
	f, err := scanFlight(s.db.QueryRowContext(ctx, `SELECT `+flightCols+` FROM flights WHERE id = $1`, id))
	return f, notFound(err, fmt.Sprintf("flight %d", id))
}

func (s *Store) SeatMap(ctx context.Context, flightID int64, now time.Time) ([]domain.Seat, error) {
	if _, err := s.GetFlight(ctx, flightID); err != nil {
		return nil, err
	}
	// The CASE expression computes the status IN SQL with the same rule as the
	// memory store: booked > held (unexpired hold) > available.
	rows, err := s.db.QueryContext(ctx, `
		SELECT seat_no, seat_row, seat_letter, cabin, price_paise,
		       CASE WHEN booking_pnr IS NOT NULL THEN 'booked'
		            WHEN hold_id IS NOT NULL AND hold_until > $2 THEN 'held'
		            ELSE 'available' END
		FROM seats WHERE flight_id = $1
		ORDER BY seat_row, seat_letter`, flightID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Seat, 0, 180)
	for rows.Next() {
		var st domain.Seat
		var cabin, status string
		if err := rows.Scan(&st.No, &st.Row, &st.Letter, &cabin, &st.PricePaise, &status); err != nil {
			return nil, err
		}
		st.Cabin, st.Status = domain.Cabin(cabin), domain.SeatStatus(status)
		out = append(out, st)
	}
	return out, rows.Err()
}

// ── HoldStore ───────────────────────────────────────────────────────────────

func (s *Store) CreateHold(ctx context.Context, h domain.Hold, now time.Time) (domain.Hold, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// STEP 1: LOCK the requested seats.
		//
		// FOR UPDATE: any other transaction wanting these rows waits until we
		// COMMIT or ROLLBACK. This closes the "check then write" race.
		//
		// ORDER BY seat_row, seat_letter: rows are locked in this order. If every
		// transaction locks in the same order, two of them can never each hold a
		// seat the other is waiting for: NO DEADLOCKS.
		rows, err := tx.QueryContext(ctx, `
			SELECT seat_no, price_paise,
			       booking_pnr IS NULL AND (hold_id IS NULL OR hold_until <= $3) AS free
			FROM seats
			WHERE flight_id = $1 AND seat_no = ANY($2)
			ORDER BY seat_row, seat_letter
			FOR UPDATE`, h.FlightID, pq.Array(h.Seats), now)
		if err != nil {
			return err
		}
		var taken []string
		var total int64
		found := 0
		for rows.Next() {
			var no string
			var price int64
			var free bool
			if err := rows.Scan(&no, &price, &free); err != nil {
				rows.Close()
				return err
			}
			found++
			if !free {
				taken = append(taken, no)
			}
			total += price
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// STEP 2: CHECK. Fewer rows than seats asked → the flight or a seat does
		// not exist. Returning an error makes inTx ROLLBACK, which also releases
		// the locks.
		if found != len(h.Seats) {
			if _, err := s.GetFlight(ctx, h.FlightID); err != nil {
				return err
			}
			return fmt.Errorf("seat: %w", domain.ErrNotFound)
		}
		if len(taken) > 0 {
			return &domain.SeatsUnavailableError{Seats: taken}
		}

		// STEP 3: WRITE. We still hold the locks, so nothing changed since the check.
		h.TotalPaise, h.Status, h.CreatedAt = total, domain.HoldActive, now
		if _, err := tx.ExecContext(ctx, `INSERT INTO holds (id, flight_id, user_id, seats, total_paise, status, expires_at, created_at)
			VALUES ($1,$2,$3,$4,$5,'active',$6,$7)`,
			h.ID, h.FlightID, h.UserID, pq.Array(h.Seats), total, h.ExpiresAt, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE seats SET hold_id = $1, hold_until = $2
			WHERE flight_id = $3 AND seat_no = ANY($4)`, h.ID, h.ExpiresAt, h.FlightID, pq.Array(h.Seats))
		return err
	})
	if err != nil {
		return domain.Hold{}, err
	}
	return h, nil
}

const holdCols = `id, flight_id, user_id, seats, total_paise, status, expires_at, created_at`

func scanHold(r interface{ Scan(...any) error }) (domain.Hold, error) {
	var h domain.Hold
	var status string
	err := r.Scan(&h.ID, &h.FlightID, &h.UserID, pq.Array(&h.Seats), &h.TotalPaise, &status, &h.ExpiresAt, &h.CreatedAt)
	h.Status = domain.HoldStatus(status)
	h.ExpiresAt, h.CreatedAt = h.ExpiresAt.UTC(), h.CreatedAt.UTC()
	return h, err
}

func (s *Store) GetHold(ctx context.Context, id string) (domain.Hold, error) {
	h, err := scanHold(s.db.QueryRowContext(ctx, `SELECT `+holdCols+` FROM holds WHERE id = $1`, id))
	return h, notFound(err, "hold "+id)
}

// lockActiveHold locks a hold row and checks it is still active.
// Used by ConfirmHold and ReleaseHold (DRY). LOCK ORDER: hold row first, then
// its seats. Every method that touches both follows this order.
func lockActiveHold(ctx context.Context, tx *sql.Tx, id string, now time.Time) (domain.Hold, error) {
	h, err := scanHold(tx.QueryRowContext(ctx, `SELECT `+holdCols+` FROM holds WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return h, notFound(err, "hold "+id)
	}
	switch h.EffectiveStatus(now) {
	case domain.HoldActive:
		return h, nil
	case domain.HoldExpired:
		return h, domain.ErrHoldExpired
	default:
		return h, fmt.Errorf("hold is %s: %w", h.Status, domain.ErrHoldNotActive)
	}
}

func (s *Store) ConfirmHold(ctx context.Context, holdID, pnr string, now time.Time) (domain.Booking, error) {
	var b domain.Booking
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		h, err := lockActiveHold(ctx, tx, holdID, now)
		if err != nil {
			return err
		}
		b = domain.Booking{PNR: pnr, HoldID: h.ID, FlightID: h.FlightID, UserID: h.UserID,
			Seats: h.Seats, TotalPaise: h.TotalPaise, Status: domain.BookingConfirmed, CreatedAt: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO bookings (pnr, hold_id, flight_id, user_id, seats, total_paise, status, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,'confirmed',$7)`,
			pnr, h.ID, h.FlightID, h.UserID, pq.Array(h.Seats), h.TotalPaise, now); err != nil {
			if isUniqueViolation(err) {
				return domain.ErrPNRTaken // the PRIMARY KEY caught a duplicate PNR
			}
			return err
		}
		// Move the seats from held to booked. Only seats still held by THIS hold.
		res, err := tx.ExecContext(ctx, `UPDATE seats SET booking_pnr = $1, hold_id = NULL, hold_until = NULL
			WHERE hold_id = $2`, pnr, h.ID)
		if err != nil {
			return err
		}
		// Paranoia check: an active, unexpired hold must still own all its seats.
		if n, _ := res.RowsAffected(); int(n) != len(h.Seats) {
			return fmt.Errorf("hold %s owns %d of %d seats: %w", h.ID, n, len(h.Seats), domain.ErrHoldNotActive)
		}
		_, err = tx.ExecContext(ctx, `UPDATE holds SET status = 'confirmed' WHERE id = $1`, h.ID)
		return err
	})
	return b, err
}

func (s *Store) ReleaseHold(ctx context.Context, holdID string, now time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := lockActiveHold(ctx, tx, holdID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE seats SET hold_id = NULL, hold_until = NULL WHERE hold_id = $1`, holdID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE holds SET status = 'released' WHERE id = $1`, holdID)
		return err
	})
}

func (s *Store) ExpireHolds(ctx context.Context, now time.Time) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// UPDATE ... RETURNING changes the rows AND tells us which ones, in one
		// round trip. The partial index holds_active_expiry makes the WHERE fast.
		rows, err := tx.QueryContext(ctx, `UPDATE holds SET status = 'expired'
			WHERE status = 'active' AND expires_at <= $1 RETURNING id`, now)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(ids) == 0 {
			return err
		}
		n = len(ids)
		_, err = tx.ExecContext(ctx, `UPDATE seats SET hold_id = NULL, hold_until = NULL WHERE hold_id = ANY($1)`, pq.Array(ids))
		return err
	})
	return n, err
}

func (s *Store) ActiveHolds(ctx context.Context) ([]domain.HoldExpiry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, expires_at FROM holds WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.HoldExpiry
	for rows.Next() {
		var e domain.HoldExpiry
		if err := rows.Scan(&e.HoldID, &e.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── BookingStore ────────────────────────────────────────────────────────────

const bookingCols = `pnr, hold_id, flight_id, user_id, seats, total_paise, status, created_at, cancelled_at`

func scanBooking(r interface{ Scan(...any) error }) (domain.Booking, error) {
	var b domain.Booking
	var status string
	var cancelled sql.NullTime // NullTime handles a NULL column (not cancelled yet)
	err := r.Scan(&b.PNR, &b.HoldID, &b.FlightID, &b.UserID, pq.Array(&b.Seats), &b.TotalPaise, &status, &b.CreatedAt, &cancelled)
	b.Status = domain.BookingStatus(status)
	b.CreatedAt = b.CreatedAt.UTC()
	if cancelled.Valid {
		t := cancelled.Time.UTC()
		b.CancelledAt = &t
	}
	return b, err
}

func (s *Store) GetBooking(ctx context.Context, pnr string) (domain.Booking, error) {
	b, err := scanBooking(s.db.QueryRowContext(ctx, `SELECT `+bookingCols+` FROM bookings WHERE pnr = $1`, pnr))
	return b, notFound(err, "booking "+pnr)
}

func (s *Store) CancelBooking(ctx context.Context, pnr string, now time.Time) (domain.Booking, error) {
	var b domain.Booking
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		b, err = scanBooking(tx.QueryRowContext(ctx, `SELECT `+bookingCols+` FROM bookings WHERE pnr = $1 FOR UPDATE`, pnr))
		if err != nil {
			return notFound(err, "booking "+pnr)
		}
		if b.Status == domain.BookingCancelled {
			return domain.ErrAlreadyCancel
		}
		if _, err := tx.ExecContext(ctx, `UPDATE seats SET booking_pnr = NULL WHERE booking_pnr = $1`, pnr); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE bookings SET status = 'cancelled', cancelled_at = $2 WHERE pnr = $1`, pnr, now); err != nil {
			return err
		}
		b.Status = domain.BookingCancelled
		t := now.UTC()
		b.CancelledAt = &t
		return nil
	})
	return b, err
}

// ── IdempotencyStore ────────────────────────────────────────────────────────

func (s *Store) Reserve(ctx context.Context, key, fingerprint string, now time.Time) (*store.SavedResponse, error) {
	// INSERT ... ON CONFLICT DO NOTHING is ATOMIC: if two retries arrive at
	// the same instant, exactly one insert succeeds (1 row affected) and the
	// other sees 0 rows. No lock or transaction of our own is needed.
	res, err := s.db.ExecContext(ctx, `INSERT INTO idempotency_keys (key, fingerprint, created_at)
		VALUES ($1,$2,$3) ON CONFLICT (key) DO NOTHING`, key, fingerprint, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, nil // we own this key now: do the work
	}
	// The key existed. Read what is stored for it.
	var fp string
	var status sql.NullInt64
	var body []byte
	if err := s.db.QueryRowContext(ctx, `SELECT fingerprint, status_code, body FROM idempotency_keys WHERE key = $1`, key).
		Scan(&fp, &status, &body); err != nil {
		return nil, err
	}
	switch {
	case fp != fingerprint:
		return nil, store.ErrKeyMismatch
	case !status.Valid:
		return nil, store.ErrKeyInProgress
	default:
		return &store.SavedResponse{Status: int(status.Int64), Body: body}, nil
	}
}

func (s *Store) Complete(ctx context.Context, key string, resp store.SavedResponse) error {
	_, err := s.db.ExecContext(ctx, `UPDATE idempotency_keys SET status_code = $2, body = $3 WHERE key = $1`, key, resp.Status, resp.Body)
	return err
}

func (s *Store) Forget(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE key = $1`, key)
	return err
}

func (s *Store) PurgeKeys(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Reset empties every table. ONLY for tests: each test starts from a clean database.
func (s *Store) Reset(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `TRUNCATE seats, bookings, holds, flights, idempotency_keys RESTART IDENTITY CASCADE`)
	return err
}
