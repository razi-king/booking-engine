package domain

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestNormalizeSeats(t *testing.T) {
	// Table-driven: each row is one scenario; the loop below is written once.
	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{"sorts by row NUMBER, not text", []string{"12A", "2B", " 3c"}, []string{"2B", "3C", "12A"}, false},
		{"letters within a row", []string{"5F", "5A", "5C"}, []string{"5A", "5C", "5F"}, false},
		{"duplicate after normalising", []string{"12a", "12A"}, nil, true},
		{"row out of range", []string{"31A"}, nil, true},
		{"row zero", []string{"0A"}, nil, true},
		{"bad letter", []string{"12G"}, nil, true},
		{"garbage", []string{"window"}, nil, true},
		{"empty", nil, nil, true},
		{"too many", []string{"1A", "1B", "1C", "1D", "1E"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeSeats(tt.in, 30, 4)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSeatPrice(t *testing.T) {
	const base = 500000 // ₹5,000
	if got := SeatPrice(base, 1, "B"); got != 2000000 {
		t.Errorf("business = %d", got)
	}
	if got := SeatPrice(base, 20, "A"); got != 535000 {
		t.Errorf("economy window = %d", got)
	}
	if got := SeatPrice(base, 4, "E"); got != 550000 {
		t.Errorf("front economy middle = %d", got)
	}
}

func TestEffectiveStatus(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	h := Hold{Status: HoldActive, ExpiresAt: now}
	if h.EffectiveStatus(now.Add(-time.Second)) != HoldActive {
		t.Error("before the deadline the hold is active")
	}
	if h.EffectiveStatus(now) != HoldExpired {
		t.Error("AT the deadline the hold has expired")
	}
	h.Status = HoldConfirmed
	if h.EffectiveStatus(now.Add(time.Hour)) != HoldConfirmed {
		t.Error("final states never change with time")
	}
}

func TestErrorsIs(t *testing.T) {
	var err error = &SeatsUnavailableError{Seats: []string{"12A"}}
	if !errors.Is(err, ErrSeatTaken) {
		t.Fatal("SeatsUnavailableError must match ErrSeatTaken")
	}
	err = &ValidationError{Fields: map[string]string{"b": "x", "a": "y"}}
	if !errors.Is(err, ErrInvalid) || err.Error() != "invalid request: a: y; b: x" {
		t.Fatalf("got %q", err)
	}
}
