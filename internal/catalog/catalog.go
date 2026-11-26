// Package catalog builds the demo flights and their seats.
//
// WHY A SEPARATE PACKAGE? Both stores (memory and Postgres) need the same
// starting data. Generating it in one place (DRY) means the two stores start
// identical, so the shared contract tests can make exact assertions.
package catalog

import (
	"time"

	"github.com/razi-king/booking-engine/internal/domain"
)

// FlightSeed is a flight plus all its seats, ready to insert.
type FlightSeed struct {
	Flight    domain.Flight
	BasePaise int64
	Seats     []domain.Seat
}

// Rows is the size of every demo aircraft: 30 rows × 6 seats = 180 seats.
const Rows = 30

// Demo returns three flights departing 7, 14 and 21 days after `now`.
//
// Dates are relative to now so the demo never shows flights in the past,
// however long after writing this you run it. Times are truncated to the day
// and set to fixed hours so they look like a real timetable.
func Demo(now time.Time) []FlightSeed {
	day := now.UTC().Truncate(24 * time.Hour)
	specs := []struct {
		number, from, to string
		afterDays, hour  int
		basePaise        int64
	}{
		{"6E-2031", "BOM", "DEL", 7, 6, 550000},   // ₹5,500
		{"AI-865", "DEL", "BLR", 14, 9, 620000},   // ₹6,200
		{"QP-1102", "BLR", "GOI", 21, 17, 380000}, // ₹3,800
	}
	out := make([]FlightSeed, 0, len(specs))
	for _, s := range specs {
		dep := day.AddDate(0, 0, s.afterDays).Add(time.Duration(s.hour) * time.Hour)
		out = append(out, FlightSeed{
			Flight:    domain.Flight{Number: s.number, From: s.from, To: s.to, Departure: dep, Rows: Rows},
			BasePaise: s.basePaise,
			Seats:     Seats(Rows, s.basePaise),
		})
	}
	return out
}

// Seats builds every seat of an aircraft, already in seat order.
// Two nested loops: rows × letters = O(rows × 6).
func Seats(rows int, basePaise int64) []domain.Seat {
	seats := make([]domain.Seat, 0, rows*len(domain.SeatLetters)) // exact capacity: no re-allocation
	for row := 1; row <= rows; row++ {
		for _, r := range domain.SeatLetters { // ranging over a string yields runes ('A', 'B', ...)
			letter := string(r)
			seats = append(seats, domain.Seat{
				No:         domain.SeatNo(row, letter),
				Row:        row,
				Letter:     letter,
				Cabin:      domain.CabinFor(row),
				PricePaise: domain.SeatPrice(basePaise, row, letter),
				Status:     domain.SeatAvailable,
			})
		}
	}
	return seats
}
