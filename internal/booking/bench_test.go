package booking

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// BenchmarkScheduleInsert compares adding n hold deadlines to:
//   - the indexed min-heap: O(log n) per insert
//   - a slice kept sorted:  O(n) per insert (binary search is fast, but the
//     elements after the insertion point must all shift right by one)
//
// Run: go test -bench Schedule -benchmem ./internal/booking
func BenchmarkScheduleInsert(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		// Deadlines in a scrambled order, as real holds arrive.
		ats := make([]time.Time, n)
		for i := range ats {
			ats[i] = time.Unix(int64((i*7919)%n), 0)
		}
		b.Run(fmt.Sprintf("heap/n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				h := newExpiryHeap()
				for j, at := range ats {
					h.add(fmt.Sprint(j), at)
				}
			}
		})
		b.Run(fmt.Sprintf("sortedSlice/n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var s []expiryItem
				for j, at := range ats {
					k := sort.Search(len(s), func(x int) bool { return s[x].at.After(at) }) // O(log n) find
					s = append(s, expiryItem{})
					copy(s[k+1:], s[k:]) // O(n) shift: this is the expensive part
					s[k] = expiryItem{holdID: fmt.Sprint(j), at: at}
				}
			}
		})
	}
}
