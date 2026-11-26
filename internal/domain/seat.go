package domain

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SeatLetters are the six seats across a narrow-body aircraft (A320/737):
//
//	A B C | aisle | D E F
//
// A and F are windows, C and D are aisles, B and E are middles.
const SeatLetters = "ABCDEF"

// ParseSeatNo turns "12A" into (12, "A").
//
// WHY PARSE AT ALL? Seat numbers arrive as text from users ("12a", " 3C").
// Parsing once into numbers lets us validate (row 0? letter G?), normalise
// (upper case) and SORT correctly (see SeatLess).
func ParseSeatNo(s string, rows int) (row int, letter string, err error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) < 2 || len(s) > 3 {
		return 0, "", fmt.Errorf("%q is not a seat number like 12A", s)
	}
	// Everything except the last character is the row; the last is the letter.
	letter = s[len(s)-1:]
	row, convErr := strconv.Atoi(s[:len(s)-1])
	switch {
	case convErr != nil:
		return 0, "", fmt.Errorf("%q: row must be a number", s)
	case row < 1 || row > rows:
		return 0, "", fmt.Errorf("%q: row must be between 1 and %d", s, rows)
	case !strings.Contains(SeatLetters, letter):
		return 0, "", fmt.Errorf("%q: letter must be one of %s", s, SeatLetters)
	}
	return row, letter, nil
}

// SeatNo is the inverse of ParseSeatNo: (12, "A") → "12A".
func SeatNo(row int, letter string) string { return strconv.Itoa(row) + letter }

// SeatLess orders seats the way people expect: by row NUMBER, then letter.
//
// WHY NOT sort.Strings? Text comparison goes character by character, so
// "12A" < "2A" (because '1' < '2'). Rows would come out 1, 10, 11, 12, 2, 3...
// Comparing the parsed row as an integer gives 1, 2, 3, ... 10, 11, 12.
//
// This order also matters for CORRECTNESS: every store locks seats in this
// order, which makes deadlocks impossible (see the docs, section 2.5).
func SeatLess(a, b string) bool {
	ra, la := splitSeat(a)
	rb, lb := splitSeat(b)
	if ra != rb {
		return ra < rb
	}
	return la < lb
}

// splitSeat is SeatLess's helper. Input is already validated, so errors are ignored.
func splitSeat(s string) (int, string) {
	r, _ := strconv.Atoi(s[:len(s)-1])
	return r, s[len(s)-1:]
}

// SortSeatNos sorts seat numbers in place in SeatLess order. O(n log n).
func SortSeatNos(seats []string) {
	sort.Slice(seats, func(i, j int) bool { return SeatLess(seats[i], seats[j]) })
}

// NormalizeSeats validates, upper-cases, de-duplicates and sorts a request's seats.
//
// DSA: a SET (Go has no set type, so map[string]struct{}) finds duplicates in
// O(n): one pass, O(1) membership checks. struct{} takes zero bytes, so the
// map stores only keys. Comparing every pair would be O(n²).
func NormalizeSeats(raw []string, rows, maxSeats int) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("choose at least one seat")
	}
	if len(raw) > maxSeats {
		return nil, fmt.Errorf("at most %d seats per hold", maxSeats)
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		row, letter, err := ParseSeatNo(s, rows)
		if err != nil {
			return nil, err
		}
		no := SeatNo(row, letter) // canonical form: "012a" and "12A" both become "12A"
		if _, dup := seen[no]; dup {
			return nil, fmt.Errorf("seat %s is listed twice", no)
		}
		seen[no] = struct{}{}
		out = append(out, no)
	}
	SortSeatNos(out)
	return out, nil
}

// CabinFor says which cabin a row belongs to. The first BusinessRows rows are business.
const BusinessRows = 2

func CabinFor(row int) Cabin {
	if row <= BusinessRows {
		return CabinBusiness
	}
	return CabinEconomy
}

// SeatPrice is the pricing rule for one seat, in paise.
//
// Keeping it in ONE function (DRY) means the seed data, both stores and the
// docs all agree. Real airlines use fare buckets that change with demand;
// fixed rules keep the demo predictable.
func SeatPrice(basePaise int64, row int, letter string) int64 {
	if CabinFor(row) == CabinBusiness {
		return basePaise * 4 // business costs four times the base fare
	}
	price := basePaise
	switch letter {
	case "A", "F": // window
		price += 35000 // ₹350
	case "C", "D": // aisle
		price += 25000 // ₹250
	}
	if row <= BusinessRows+3 { // the first few economy rows: get off the plane first
		price += 50000 // ₹500
	}
	return price
}
