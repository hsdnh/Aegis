package probe

import (
	"encoding/binary"
	"time"
	"unsafe"
)

// Span represents a single function execution trace.
// Fixed 128 bytes — no pointers, no allocations, GC-invisible.
type Span struct {
	FuncID    uint32    // index into function name table (not a string — zero alloc)
	ParentID  uint32    // links to caller span
	GoroutineID uint64  // which goroutine
	StartNano int64     // monotonic nanoseconds
	EndNano   int64     // monotonic nanoseconds
	Flags     uint16    // bit flags: hasError, isAsync, etc.
	Depth     uint16    // call depth
	_pad      [88]byte  // pad to exactly 128 bytes for cache-line alignment
}

// SpanSize is the exact byte size of a Span. Must be power of 2 for ring buffer.
const SpanSize = int(unsafe.Sizeof(Span{}))

// Flag constants
const (
	FlagHasError  uint16 = 1 << 0
	FlagIsAsync   uint16 = 1 << 1
	FlagIsSlow    uint16 = 1 << 2 // exceeded expected duration
	FlagIsDB      uint16 = 1 << 3
	FlagIsRedis   uint16 = 1 << 4
	FlagIsHTTP    uint16 = 1 << 5
	FlagIsRPC     uint16 = 1 << 6
)

// Duration returns the span duration.
func (s *Span) Duration() time.Duration {
	return time.Duration(s.EndNano - s.StartNano)
}

// SetError marks this span as having an error.
func (s *Span) SetError() {
	s.Flags |= FlagHasError
}

// FuncEntry is a mapping from FuncID to function metadata.
// Sent once at registration, not per-span — saves bandwidth.
type FuncEntry struct {
	ID       uint32
	Name     string // "main.OrderCreate"
	File     string // "order/create.go"
	Line     int32
	Package  string // "main"
	Priority byte   // 0=skip, 1=timing_only, 2=full_trace
}

// WindowSnapshot is the complete data from one trace window.
type WindowSnapshot struct {
	WindowID  uint64        `json:"window_id"`
	StartTime time.Time     `json:"start_time"`
	Duration  time.Duration `json:"duration"`
	Trigger   string        `json:"trigger"` // "scheduled", "anomaly", "manual"
	SpanCount int           `json:"span_count"`
	Spans     []byte        `json:"spans"` // raw span bytes, decoded by agent
	// Runtime stats captured during window
	Goroutines    int   `json:"goroutines"`
	HeapAllocMB   int   `json:"heap_alloc_mb"`
	GCPauseNs     int64 `json:"gc_pause_ns"`
	MutexWaitNs   int64 `json:"mutex_wait_ns"`
}

// SerializeSpan writes a span to bytes without allocation.
func SerializeSpan(s *Span, buf []byte) {
	binary.LittleEndian.PutUint32(buf[0:4], s.FuncID)
	binary.LittleEndian.PutUint32(buf[4:8], s.ParentID)
	binary.LittleEndian.PutUint64(buf[8:16], s.GoroutineID)
	binary.LittleEndian.PutUint64(buf[16:24], uint64(s.StartNano))
	binary.LittleEndian.PutUint64(buf[24:32], uint64(s.EndNano))
	binary.LittleEndian.PutUint16(buf[32:34], s.Flags)
	binary.LittleEndian.PutUint16(buf[34:36], s.Depth)
}

// DeserializeSpan reads a span from bytes.
func DeserializeSpan(buf []byte) Span {
	return Span{
		FuncID:      binary.LittleEndian.Uint32(buf[0:4]),
		ParentID:    binary.LittleEndian.Uint32(buf[4:8]),
		GoroutineID: binary.LittleEndian.Uint64(buf[8:16]),
		StartNano:   int64(binary.LittleEndian.Uint64(buf[16:24])),
		EndNano:     int64(binary.LittleEndian.Uint64(buf[24:32])),
		Flags:       binary.LittleEndian.Uint16(buf[32:34]),
		Depth:       binary.LittleEndian.Uint16(buf[34:36]),
	}
}
