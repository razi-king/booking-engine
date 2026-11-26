package booking

import (
	"container/heap"
	"time"
)

// expiryHeap is an INDEXED MIN-HEAP of holds ordered by deadline.
//
// WHAT IT GIVES US:
//   - Peek at the hold that expires SOONEST:            O(1)  (always index 0)
//   - Add a hold:                                         O(log n)
//   - Remove ANY hold by id (confirmed/released early):  O(log n)
//
// WHY A HEAP? The scheduler only ever asks "what is the next deadline?". A
// sorted slice answers that in O(1) too, but inserting into the middle of a
// slice shifts up to n elements: O(n). A heap keeps just enough order (each
// parent ≤ its children) to know the minimum, so inserts are O(log n).
//
// WHY "INDEXED"? A plain heap can only remove its top. When a user confirms a
// hold early we want to remove THAT hold. The `pos` map remembers where every
// hold sits in the array, so heap.Remove can take it out in O(log n) instead
// of searching the array in O(n). Every swap updates pos (see Swap).
type expiryHeap struct {
	items []expiryItem
	pos   map[string]int // hold id → index in items
}

type expiryItem struct {
	holdID string
	at     time.Time
}

func newExpiryHeap() *expiryHeap { return &expiryHeap{pos: map[string]int{}} }

// The five methods below implement heap.Interface, so Go's container/heap can
// do the sift-up / sift-down algorithms for us.

func (h *expiryHeap) Len() int           { return len(h.items) }
func (h *expiryHeap) Less(i, j int) bool { return h.items[i].at.Before(h.items[j].at) } // earlier = higher priority
func (h *expiryHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.pos[h.items[i].holdID] = i // keep the index map in step with every move
	h.pos[h.items[j].holdID] = j
}
func (h *expiryHeap) Push(x any) {
	it := x.(expiryItem)
	h.pos[it.holdID] = len(h.items)
	h.items = append(h.items, it)
}
func (h *expiryHeap) Pop() any {
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	delete(h.pos, last.holdID)
	return last
}

// add inserts a hold, or moves it if it is already there.
func (h *expiryHeap) add(id string, at time.Time) {
	if i, ok := h.pos[id]; ok {
		h.items[i].at = at
		heap.Fix(h, i) // deadline changed: restore heap order from index i
		return
	}
	heap.Push(h, expiryItem{holdID: id, at: at})
}

// remove deletes a hold if present.
func (h *expiryHeap) remove(id string) {
	if i, ok := h.pos[id]; ok {
		heap.Remove(h, i)
	}
}

// next returns the earliest deadline, if any.
func (h *expiryHeap) next() (time.Time, bool) {
	if len(h.items) == 0 {
		return time.Time{}, false
	}
	return h.items[0].at, true
}

// popDue removes every hold whose deadline is at or before now and returns how many.
func (h *expiryHeap) popDue(now time.Time) int {
	n := 0
	for len(h.items) > 0 && !h.items[0].at.After(now) {
		heap.Pop(h)
		n++
	}
	return n
}
