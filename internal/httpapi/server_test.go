package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/razi-king/booking-engine/internal/booking"
	"github.com/razi-king/booking-engine/internal/catalog"
	"github.com/razi-king/booking-engine/internal/store/memory"
)

type noTracker struct{}

func (noTracker) Track(string, time.Time) {}
func (noTracker) Forget(string)           {}

// newAPI builds the real router over a real service and memory store:
// an end-to-end test of everything except the network.
func newAPI(t *testing.T) http.Handler {
	t.Helper()
	st := memory.New()
	st.Seed(context.Background(), catalog.Demo(time.Now()))
	svc := booking.NewService(st, noTracker{}, booking.Config{HoldTTL: 10 * time.Minute, MaxSeats: 9, PNRAttempts: 3}, nil)
	return NewServer(svc, st, nil, nil, nil).Routes()
}

// send is a DRY helper: one request in, recorder out.
func send(t *testing.T, h http.Handler, method, path, body, userID, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if userID != "" {
		req.Header.Set(HeaderUser, userID)
	}
	if key != "" {
		req.Header.Set(HeaderIdemKey, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEndToEnd(t *testing.T) {
	api := newAPI(t)
	rec := send(t, api, "POST", "/api/v1/holds", `{"flightId":1,"seats":["12A","12B"]}`, "asha", "")
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") == "" {
		t.Fatalf("hold: %d %s", rec.Code, rec.Body)
	}
	var h struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &h)

	// Ben tries 12B: 409 and the body names the taken seat.
	rec = send(t, api, "POST", "/api/v1/holds", `{"flightId":1,"seats":["12B"]}`, "ben", "")
	var eb ErrorBody
	json.Unmarshal(rec.Body.Bytes(), &eb)
	if rec.Code != http.StatusConflict || eb.Error != "seat_unavailable" || eb.Seats[0] != "12B" {
		t.Fatalf("conflict: %d %+v", rec.Code, eb)
	}

	rec = send(t, api, "POST", "/api/v1/holds/"+h.ID+"/confirm", "", "asha", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
	var b struct{ PNR string }
	json.Unmarshal(rec.Body.Bytes(), &b)
	if rec := send(t, api, "GET", "/api/v1/bookings/"+b.PNR, "", "asha", ""); rec.Code != http.StatusOK {
		t.Fatalf("get booking: %d", rec.Code)
	}
	if rec := send(t, api, "GET", "/api/v1/bookings/"+b.PNR, "", "ben", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger gets booking: %d", rec.Code)
	}
}

func TestIdempotentConfirmIsReplayed(t *testing.T) {
	api := newAPI(t)
	rec := send(t, api, "POST", "/api/v1/holds", `{"flightId":1,"seats":["5A"]}`, "asha", "")
	var h struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &h)

	// The app sends confirm; the response is "lost"; the app retries with the SAME key.
	first := send(t, api, "POST", "/api/v1/holds/"+h.ID+"/confirm", "", "asha", "key-123")
	retry := send(t, api, "POST", "/api/v1/holds/"+h.ID+"/confirm", "", "asha", "key-123")
	if first.Code != http.StatusCreated || retry.Code != http.StatusCreated {
		t.Fatalf("codes %d / %d", first.Code, retry.Code)
	}
	if first.Body.String() != retry.Body.String() || retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("the retry must get the SAME booking back, marked as replayed")
	}
	// WITHOUT a key, a second confirm is a real second attempt and is refused.
	if rec := send(t, api, "POST", "/api/v1/holds/"+h.ID+"/confirm", "", "asha", ""); rec.Code != http.StatusConflict {
		t.Fatalf("second confirm without key: %d", rec.Code)
	}
}

func TestIdempotencyKeyReuseWithDifferentBody(t *testing.T) {
	api := newAPI(t)
	send(t, api, "POST", "/api/v1/holds", `{"flightId":1,"seats":["6A"]}`, "asha", "k")
	rec := send(t, api, "POST", "/api/v1/holds", `{"flightId":1,"seats":["6B"]}`, "asha", "k")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422", rec.Code)
	}
	// The same key from ANOTHER user is a different key (scoped per user).
	if rec := send(t, api, "POST", "/api/v1/holds", `{"flightId":1,"seats":["6B"]}`, "ben", "k"); rec.Code != http.StatusCreated {
		t.Fatalf("other user, same key: %d", rec.Code)
	}
}

func TestBadRequests(t *testing.T) {
	api := newAPI(t)
	cases := []struct {
		name, method, path, body, user string
		want                           int
	}{
		{"no user", "POST", "/api/v1/holds", `{"flightId":1,"seats":["1A"]}`, "", 401},
		{"unknown field", "POST", "/api/v1/holds", `{"flightId":1,"seat":["1A"]}`, "u", 400},
		{"bad seat", "POST", "/api/v1/holds", `{"flightId":1,"seats":["99Z"]}`, "u", 400},
		{"bad flight id", "GET", "/api/v1/flights/abc/seats", "", "", 400},
		{"unknown flight", "GET", "/api/v1/flights/9/seats", "", "", 404},
		{"unknown hold", "POST", "/api/v1/holds/nope/confirm", "", "u", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := send(t, api, c.method, c.path, c.body, c.user, ""); rec.Code != c.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
		})
	}
}
