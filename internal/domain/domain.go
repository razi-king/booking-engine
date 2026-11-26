// Package domain holds the words of the business: flights, seats, holds and
// bookings, plus the rules that are true no matter where data is stored.
//
// It imports nothing from this project. Every other package depends on it,
// never the other way round, so the core rules stay stable while storage,
// HTTP and configuration can change around them.
package domain

import "time"

// Cabin is the class of travel. A named string type (instead of plain string)
// means the compiler stops us passing, say, a seat number where a cabin is expected.
type Cabin string

const (
	CabinBusiness Cabin = "business"
	CabinEconomy  Cabin = "economy"
)

// SeatStatus is what the seat map shows for one seat. It is COMPUTED from
// stored facts (is there a booking? an unexpired hold?) rather than stored,
// so it can never disagree with them.
type SeatStatus string

const (
	SeatAvailable SeatStatus = "available"
	SeatHeld      SeatStatus = "held"
	SeatBooked    SeatStatus = "booked"
)

// HoldStatus is the life cycle of a hold:
//
//	active ──confirm──► confirmed
//	   │ ──release──► released
//	   └──time passes──► expired
//
// Only "active" can change; the other three are final.
type HoldStatus string

const (
	HoldActive    HoldStatus = "active"
	HoldConfirmed HoldStatus = "confirmed"
	HoldReleased  HoldStatus = "released"
	HoldExpired   HoldStatus = "expired"
)

// BookingStatus is the life cycle of a booking: confirmed → cancelled.
type BookingStatus string

const (
	BookingConfirmed BookingStatus = "confirmed"
	BookingCancelled BookingStatus = "cancelled"
)

// Flight is one departure of one flight number.
type Flight struct {
	ID        int64     `json:"id"`
	Number    string    `json:"number"` // e.g. "6E-2031"
	From      string    `json:"from"`   // IATA code
	To        string    `json:"to"`
	Departure time.Time `json:"departure"`
	Rows      int       `json:"rows"` // how many seat rows the aircraft has
}

// Seat is one seat on one flight, as shown on the seat map.
type Seat struct {
	No         string     `json:"seat"` // "12A"
	Row        int        `json:"row"`
	Letter     string     `json:"letter"`
	Cabin      Cabin      `json:"cabin"`
	PricePaise int64      `json:"pricePaise"` // money in whole paise: integers are exact, floats are not
	Status     SeatStatus `json:"status"`
}

// Hold reserves seats for one user until ExpiresAt.
type Hold struct {
	ID         string     `json:"id"`
	FlightID   int64      `json:"flightId"`
	UserID     string     `json:"userId"`
	Seats      []string   `json:"seats"` // always in seat order (see SortSeatNos)
	TotalPaise int64      `json:"totalPaise"`
	Status     HoldStatus `json:"status"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// EffectiveStatus returns the status taking time into account. A hold stored
// as "active" whose deadline has passed IS expired, even if the background
// sweeper has not got to it yet. This "lazy expiry" means correctness never
// depends on the sweeper running on time.
func (h Hold) EffectiveStatus(now time.Time) HoldStatus {
	if h.Status == HoldActive && !now.Before(h.ExpiresAt) {
		return HoldExpired
	}
	return h.Status
}

// Booking is a confirmed purchase, identified by its PNR.
type Booking struct {
	PNR         string        `json:"pnr"` // 6-character Passenger Name Record, e.g. "K7QX2M"
	HoldID      string        `json:"holdId"`
	FlightID    int64         `json:"flightId"`
	UserID      string        `json:"userId"`
	Seats       []string      `json:"seats"`
	TotalPaise  int64         `json:"totalPaise"`
	Status      BookingStatus `json:"status"`
	CreatedAt   time.Time     `json:"createdAt"`
	CancelledAt *time.Time    `json:"cancelledAt,omitempty"` // pointer: nil means "not cancelled"
}

// HoldExpiry is the minimum the expiry scheduler needs to know about a hold.
type HoldExpiry struct {
	HoldID    string
	ExpiresAt time.Time
}
