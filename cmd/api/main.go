// Command api runs the booking engine HTTP server.
//
// This file is the COMPOSITION ROOT: the only place that knows the concrete
// types (memory or Postgres, real clock, real scheduler) and wires them
// together. Everything else depends on interfaces.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/razi-king/booking-engine/internal/booking"
	"github.com/razi-king/booking-engine/internal/catalog"
	"github.com/razi-king/booking-engine/internal/config"
	"github.com/razi-king/booking-engine/internal/httpapi"
	"github.com/razi-king/booking-engine/internal/store"
	"github.com/razi-king/booking-engine/internal/store/memory"
	"github.com/razi-king/booking-engine/internal/store/postgres"
	"github.com/razi-king/booking-engine/web"
)

// fullStore is the combination main needs; both stores satisfy it.
type fullStore interface {
	store.Store
	store.IdempotencyStore
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg := config.Load()

	// ctx is cancelled on Ctrl+C or SIGTERM (what Docker/Kubernetes send on stop).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1. Pick the store.
	var st fullStore
	var health func(context.Context) error
	if cfg.DatabaseURL != "" {
		pg, err := postgres.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			slog.Error("postgres", "err", err)
			os.Exit(1)
		}
		defer pg.Close()
		st, health = pg, pg.Ping
		slog.Info("using postgres store")
	} else {
		st = memory.New()
		slog.Info("using in-memory store (set DATABASE_URL for postgres)")
	}
	if err := st.Seed(ctx, catalog.Demo(time.Now())); err != nil {
		slog.Error("seed", "err", err)
		os.Exit(1)
	}

	// 2. Background work: the expiry scheduler and the idempotency-key purge.
	sched := booking.NewScheduler(st, nil, cfg.SweepFallback)
	go sched.Run(ctx)
	go purgeLoop(ctx, st, cfg.IdemRetention)

	// 3. Service and HTTP.
	svc := booking.NewService(st, sched, booking.Config{HoldTTL: cfg.HoldTTL, MaxSeats: cfg.MaxSeats, PNRAttempts: 5}, nil)
	metrics := func() map[string]any {
		sweeps, expired := sched.Stats()
		return map[string]any{"sweeps": sweeps, "holdsExpired": expired}
	}
	api := httpapi.NewServer(svc, st, nil, health, metrics).Routes()
	api.Handle("GET /", http.FileServerFS(web.Files)) // the demo seat-map page

	handler := httpapi.Chain(api, httpapi.WithRequestID, httpapi.WithRecover, httpapi.WithLogging, httpapi.WithCORS(cfg.AllowedOrigins))
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second, // stops "slowloris" clients holding connections open
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		slog.Info("listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			stop()
		}
	}()

	<-ctx.Done() // wait for the shutdown signal
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx) // finish in-flight requests, accept no new ones
}

// purgeLoop deletes old idempotency keys once an hour, so the table does not grow forever.
func purgeLoop(ctx context.Context, s store.IdempotencyStore, keep time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n, err := s.PurgeKeys(ctx, now.Add(-keep)); err != nil {
				slog.Error("purge idempotency keys", "err", err)
			} else if n > 0 {
				slog.Info("purged idempotency keys", "count", n)
			}
		}
	}
}
