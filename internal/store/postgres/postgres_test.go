package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/razi-king/booking-engine/internal/store/storetest"
)

// The SAME contract suite, against a real PostgreSQL.
//
// It needs TEST_DATABASE_URL (CI starts a Postgres service and sets it). On a
// laptop without Postgres the test is SKIPPED rather than failed, so
// `go test ./...` always works.
func TestContract(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres contract tests")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	storetest.Run(t, func(t *testing.T) storetest.Full {
		// Sub-tests share one database, so each starts by emptying it.
		if err := s.Reset(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	})
}
