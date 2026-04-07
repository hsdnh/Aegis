package probe

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// RequestTrace records the complete journey of a single request through the system.
// Every function it touches, every Redis/MySQL call, every result — all linked by traceID.
type RequestTrace struct {
	TraceID   string       `json:"trace_id"`
	StartTime time.Time    `json:"start_time"`
	EndTime   time.Time    `json:"end_time,omitempty"`
	Duration  int64        `json:"duration_ns,omitempty"`
	EntryFunc string       `json:"entry_func"` // e.g. "OrderCreate"
	EntryType string       `json:"entry_type"` // "http", "cron", "consumer"
	Status    string       `json:"status"`     // "ok", "error", "slow"
	Steps     []TraceStep  `json:"steps"`
}

// TraceStep is one operation within a request trace.
type TraceStep struct {
	SeqID     int       `json:"seq_id"`     // order within this trace
	FuncName  string    `json:"func_name"`
	FuncID    uint32    `json:"func_id"`
	Depth     int       `json:"depth"`       // call depth
	StartNano int64     `json:"start_nano"`
	EndNano   int64     `json:"end_nano,omitempty"`
	DurationNs int64    `json:"duration_ns,omitempty"`
	OpType    string    `json:"op_type,omitempty"` // "redis", "mysql", "http", "func"
	OpDetail  string    `json:"op_detail,omitempty"` // "GET stock:m123" or "INSERT proxy_orders"
	OpResult  string    `json:"op_result,omitempty"` // "3" or "1 row affected" or "200 OK"
	HasError  bool      `json:"has_error,omitempty"`
	ErrorMsg  string    `json:"error_msg,omitempty"`
}

// --- Active trace storage ---

// TraceStore holds in-flight and recently completed traces.
type TraceStore struct {
	mu        sync.RWMutex
	active    map[string]*RequestTrace // traceID → in-flight trace
	completed []RequestTrace           // ring buffer of completed traces
	maxKeep   int
	seqGen    uint64
}

func NewTraceStore(maxKeep int) *TraceStore {
	if maxKeep == 0 {
		maxKeep = 200
	}
	return &TraceStore{
		active:  make(map[string]*RequestTrace),
		maxKeep: maxKeep,
	}
}

// BeginTrace starts recording a new request trace.
// Call this at HTTP handler entry or consumer function entry.
// Returns traceID for passing through the call chain.
func (ts *TraceStore) BeginTrace(entryFunc, entryType string) string {
	traceID := generateTraceID()
	trace := &RequestTrace{
		TraceID:   traceID,
		StartTime: time.Now(),
		EntryFunc: entryFunc,
		EntryType: entryType,
		Status:    "ok",
	}

	ts.mu.Lock()
	ts.active[traceID] = trace
	ts.mu.Unlock()

	return traceID
}

// AddStep records one function call or operation within a trace.
func (ts *TraceStore) AddStep(traceID string, funcName string, funcID uint32, depth int, opType, opDetail string) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	trace, ok := ts.active[traceID]
	if !ok {
		return -1
	}

	seq := int(atomic.AddUint64(&ts.seqGen, 1))
	step := TraceStep{
		SeqID:     seq,
		FuncName:  funcName,
		FuncID:    funcID,
		Depth:     depth,
		StartNano: time.Now().UnixNano(),
		OpType:    opType,
		OpDetail:  opDetail,
	}
	trace.Steps = append(trace.Steps, step)
	return len(trace.Steps) - 1 // index for CompleteStep
}

// CompleteStep fills in the end time and result for a step.
func (ts *TraceStore) CompleteStep(traceID string, stepIdx int, result string, hasError bool, errMsg string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	trace, ok := ts.active[traceID]
	if !ok || stepIdx < 0 || stepIdx >= len(trace.Steps) {
		return
	}

	step := &trace.Steps[stepIdx]
	step.EndNano = time.Now().UnixNano()
	step.DurationNs = step.EndNano - step.StartNano
	step.OpResult = result
	step.HasError = hasError
	step.ErrorMsg = errMsg

	if hasError {
		trace.Status = "error"
	}
}

// EndTrace completes a request trace and moves it to the completed buffer.
func (ts *TraceStore) EndTrace(traceID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	trace, ok := ts.active[traceID]
	if !ok {
		return
	}

	trace.EndTime = time.Now()
	trace.Duration = trace.EndTime.Sub(trace.StartTime).Nanoseconds()

	// Mark slow if > 1 second
	if trace.Duration > 1_000_000_000 {
		if trace.Status == "ok" {
			trace.Status = "slow"
		}
	}

	// Move to completed ring buffer
	ts.completed = append(ts.completed, *trace)
	if len(ts.completed) > ts.maxKeep {
		ts.completed = ts.completed[len(ts.completed)-ts.maxKeep:]
	}
	delete(ts.active, traceID)
}

// RecentTraces returns the last N completed traces (newest first).
func (ts *TraceStore) RecentTraces(n int) []RequestTrace {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	total := len(ts.completed)
	if n > total {
		n = total
	}
	result := make([]RequestTrace, n)
	for i := 0; i < n; i++ {
		result[i] = ts.completed[total-1-i]
	}
	return result
}

// GetTrace returns a specific trace by ID.
func (ts *TraceStore) GetTrace(traceID string) *RequestTrace {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	// Check active first
	if t, ok := ts.active[traceID]; ok {
		return t
	}
	// Check completed
	for i := len(ts.completed) - 1; i >= 0; i-- {
		if ts.completed[i].TraceID == traceID {
			return &ts.completed[i]
		}
	}
	return nil
}

// Stats returns summary statistics for recent traces.
type TraceStats struct {
	TotalTraces int     `json:"total_traces"`
	OKCount     int     `json:"ok_count"`
	ErrorCount  int     `json:"error_count"`
	SlowCount   int     `json:"slow_count"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	P50DurationMs float64 `json:"p50_duration_ms"`
	P99DurationMs float64 `json:"p99_duration_ms"`
	ActiveCount int     `json:"active_count"` // currently in-flight
}

func (ts *TraceStore) Stats() TraceStats {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	stats := TraceStats{
		TotalTraces: len(ts.completed),
		ActiveCount: len(ts.active),
	}

	if len(ts.completed) == 0 {
		return stats
	}

	var totalDur int64
	durations := make([]int64, 0, len(ts.completed))
	for _, t := range ts.completed {
		switch t.Status {
		case "ok":
			stats.OKCount++
		case "error":
			stats.ErrorCount++
		case "slow":
			stats.SlowCount++
		}
		totalDur += t.Duration
		durations = append(durations, t.Duration)
	}

	stats.AvgDurationMs = float64(totalDur) / float64(len(ts.completed)) / 1e6

	// Simple percentile (no sort needed for approximate)
	if len(durations) > 0 {
		stats.P50DurationMs = float64(durations[len(durations)/2]) / 1e6
		p99idx := int(float64(len(durations)) * 0.99)
		if p99idx >= len(durations) {
			p99idx = len(durations) - 1
		}
		stats.P99DurationMs = float64(durations[p99idx]) / 1e6
	}

	return stats
}

func generateTraceID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("tr-%s-%s", time.Now().Format("150405"), hex.EncodeToString(b))
}
