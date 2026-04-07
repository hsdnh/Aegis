package probe

import (
	"sync/atomic"
)

// RingBuffer is a lock-free, fixed-size circular buffer for spans.
// Zero allocation after init. Old entries are silently overwritten.
// Like a surveillance camera loop — always recording, never growing.
const ringSize = 4096 // must be power of 2

type RingBuffer struct {
	buf  [ringSize]Span
	head uint64 // atomically incremented, wraps naturally
}

// Write stores a span. Lock-free, never blocks, never allocates.
// If buffer is full, oldest entry is overwritten (by design).
func (r *RingBuffer) Write(s Span) {
	pos := atomic.AddUint64(&r.head, 1) - 1
	r.buf[pos&(ringSize-1)] = s
}

// Drain copies all spans written since last drain and resets.
// Returns the spans and count. Caller owns the returned slice.
// Only called once per window close — not on hot path.
func (r *RingBuffer) Drain() ([]Span, int) {
	head := atomic.LoadUint64(&r.head)
	count := int(head)
	if count > ringSize {
		count = ringSize
	}
	if count == 0 {
		return nil, 0
	}

	result := make([]Span, count)
	start := head - uint64(count)
	for i := 0; i < count; i++ {
		result[i] = r.buf[(start+uint64(i))&(ringSize-1)]
	}

	// Reset head (safe because drain is only called when probe is off)
	atomic.StoreUint64(&r.head, 0)
	return result, count
}

// Count returns how many spans have been written (may exceed ringSize).
func (r *RingBuffer) Count() uint64 {
	return atomic.LoadUint64(&r.head)
}
