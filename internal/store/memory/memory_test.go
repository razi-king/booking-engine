package memory

import (
	"testing"

	"github.com/razi-king/booking-engine/internal/store/storetest"
)

// The whole contract suite, against the in-memory store.
func TestContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) storetest.Full { return New() })
}
