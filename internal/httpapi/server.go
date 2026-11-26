package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/razi-king/booking-engine/internal/booking"
	"github.com/razi-king/booking-engine/internal/domain"
	"github.com/razi-king/booking-engine/internal/store"
)

// Booker is what the handlers need from the booking service (Dependency
// Inversion: handlers depend on this interface, not on *booking.Service).
type Booker interface {
	ListFlights(ctx context.Context) ([]domain.Flight, error)
	FlightSeatMap(ctx context.Context, flightID int64) (booking.SeatMap, error)
	Hold(ctx context.Context, userID string, req booking.HoldRequest) (domain.Hold, error)
	GetHold(ctx context.Context, userID, holdID string) (domain.Hold, error)
	Confirm(ctx context.Context, userID, holdID string) (domain.Booking, error)
	Release(ctx context.Context, userID, holdID string) error
	GetBooking(ctx context.Context, userID, pnr string) (domain.Booking, error)
	Cancel(ctx context.Context, userID, pnr string) (domain.Booking, error)
}

// Server holds the handlers' dependencies.
type Server struct {
	svc     Booker
	idem    store.IdempotencyStore
	now     func() time.Time
	health  func(context.Context) error // nil = always healthy
	metrics func() map[string]any
}

// NewServer builds a server. health and metrics may be nil.
func NewServer(svc Booker, idem store.IdempotencyStore, now func() time.Time,
	health func(context.Context) error, metrics func() map[string]any) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{svc: svc, idem: idem, now: now, health: health, metrics: metrics}
}

// Routes registers every endpoint. Go 1.22+ patterns: "METHOD /path/{name}".
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	idem := WithIdempotency(s.idem, s.now) // only state-changing POSTs need it

	mux.HandleFunc("GET /api/v1/flights", s.listFlights)
	mux.HandleFunc("GET /api/v1/flights/{id}/seats", s.seatMap)
	mux.Handle("POST /api/v1/holds", idem(http.HandlerFunc(s.createHold)))
	mux.HandleFunc("GET /api/v1/holds/{id}", s.getHold)
	mux.Handle("POST /api/v1/holds/{id}/confirm", idem(http.HandlerFunc(s.confirm)))
	mux.HandleFunc("DELETE /api/v1/holds/{id}", s.release)
	mux.HandleFunc("GET /api/v1/bookings/{pnr}", s.getBooking)
	mux.Handle("POST /api/v1/bookings/{pnr}/cancel", idem(http.HandlerFunc(s.cancel)))
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.metricsHandler)
	return mux
}

// user reads the caller's id from the header (see HeaderUser).
func user(r *http.Request) string { return r.Header.Get(HeaderUser) }

// requireUser writes a 401 when the header is missing. Returns false if it did.
func requireUser(w http.ResponseWriter, r *http.Request) bool {
	if user(r) == "" {
		writeJSON(w, http.StatusUnauthorized, ErrorBody{Error: "unauthenticated", Message: "send your user id in the X-User-ID header"})
		return false
	}
	return true
}

func (s *Server) listFlights(w http.ResponseWriter, r *http.Request) {
	flights, err := s.svc.ListFlights(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flights": flights})
}

func (s *Server) seatMap(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64) // PathValue reads {id} from the pattern
	if err != nil {
		writeError(w, &domain.ValidationError{Fields: map[string]string{"id": "flight id must be a number"}})
		return
	}
	m, err := s.svc.FlightSeatMap(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) createHold(w http.ResponseWriter, r *http.Request) {
	if !requireUser(w, r) {
		return
	}
	var req booking.HoldRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields() // a typo like "seat" instead of "seats" is an error, not silently ignored
	if err := dec.Decode(&req); err != nil {
		writeError(w, &domain.ValidationError{Fields: map[string]string{"body": "expected JSON like {\"flightId\":1,\"seats\":[\"12A\"]}"}})
		return
	}
	h, err := s.svc.Hold(r.Context(), user(r), req)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/holds/"+h.ID) // REST convention: where the new thing lives
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) getHold(w http.ResponseWriter, r *http.Request) {
	if !requireUser(w, r) {
		return
	}
	h, err := s.svc.GetHold(r.Context(), user(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	if !requireUser(w, r) {
		return
	}
	b, err := s.svc.Confirm(r.Context(), user(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	if !requireUser(w, r) {
		return
	}
	if err := s.svc.Release(r.Context(), user(r), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent) // 204: done, nothing to say
}

func (s *Server) getBooking(w http.ResponseWriter, r *http.Request) {
	if !requireUser(w, r) {
		return
	}
	b, err := s.svc.GetBooking(r.Context(), user(r), r.PathValue("pnr"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if !requireUser(w, r) {
		return
	}
	b, err := s.svc.Cancel(r.Context(), user(r), r.PathValue("pnr"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.health != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.health(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	m := map[string]any{}
	if s.metrics != nil {
		m = s.metrics()
	}
	writeJSON(w, http.StatusOK, m)
}
