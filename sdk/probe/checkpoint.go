package probe

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Checkpoint validates business assertions at function boundaries.
// Only active during sampling windows — zero overhead when dormant.
//
// Usage in instrumented code (auto-generated from //aiops:check annotations):
//
//	if __aiops_p.CheckActive() {
//	    __aiops_p.Check("SearchItems", "result.all(created < 30m)", func() bool {
//	        return allItemsRecent(result, 30*time.Minute)
//	    }, map[string]string{"item_count": fmt.Sprint(len(result))})
//	}
type Checkpoint struct {
	active     uint32 // atomic: same as probe window — 0=off, 1=on
	mu         sync.Mutex
	violations []Violation
	maxKeep    int
	stats      CheckpointStats
}

// Violation records a failed business assertion.
type Violation struct {
	ID         string            `json:"id"`
	Timestamp  time.Time         `json:"timestamp"`
	FuncName   string            `json:"func_name"`
	Rule       string            `json:"rule"`       // the assertion expression
	Message    string            `json:"message"`
	Severity   string            `json:"severity"`   // "warning", "critical"
	Context    map[string]string `json:"context"`    // captured variable snapshots
	StackHint  string            `json:"stack_hint"` // file:line
}

// CheckpointStats tracks assertion pass/fail rates.
type CheckpointStats struct {
	TotalChecks   int64 `json:"total_checks"`
	PassCount     int64 `json:"pass_count"`
	FailCount     int64 `json:"fail_count"`
	WindowsRun    int64 `json:"windows_run"`
	LastWindowAt  time.Time `json:"last_window_at"`
}

// GlobalCheckpoint is the singleton checkpoint instance.
var GlobalCheckpoint = &Checkpoint{maxKeep: 200}

// CheckActive returns true if assertions should run this call.
// Same atomic load as probe — 1ns when off.
func (c *Checkpoint) CheckActive() bool {
	return atomic.LoadUint32(&c.active) > 0
}

// Check executes a business assertion. Only called when CheckActive() == true.
// predicate returns true if data is valid, false if violated.
func (c *Checkpoint) Check(funcName, rule string, predicate func() bool, ctx map[string]string) {
	// Wrap in recover — assertion code must never crash the host
	defer func() { recover() }()

	atomic.AddInt64(&c.stats.TotalChecks, 1)

	passed := predicate()
	if passed {
		atomic.AddInt64(&c.stats.PassCount, 1)
		return
	}

	// Violation detected
	atomic.AddInt64(&c.stats.FailCount, 1)

	v := Violation{
		ID:        fmt.Sprintf("chk-%d", time.Now().UnixNano()),
		Timestamp: time.Now(),
		FuncName:  funcName,
		Rule:      rule,
		Message:   fmt.Sprintf("Assertion failed in %s: %s", funcName, rule),
		Severity:  "critical",
		Context:   ctx,
	}

	c.mu.Lock()
	c.violations = append(c.violations, v)
	if len(c.violations) > c.maxKeep {
		c.violations = c.violations[len(c.violations)-c.maxKeep:]
	}
	c.mu.Unlock()

	// Also write to the probe ring buffer for the agent to pick up
	if Global.Active() {
		s := Span{
			FuncID:    0xFFFF0001, // special marker: checkpoint violation
			StartNano: time.Now().UnixNano(),
			Flags:     FlagHasError,
		}
		Global.ring.Write(s)
	}
}

// StartWindow activates assertions for a duration. Syncs with probe windows.
func (c *Checkpoint) StartWindow(duration time.Duration) {
	if !atomic.CompareAndSwapUint32(&c.active, 0, 1) {
		return
	}
	atomic.AddInt64(&c.stats.WindowsRun, 1)
	c.stats.LastWindowAt = time.Now()

	go func() {
		defer func() { recover() }()
		time.Sleep(duration)
		atomic.StoreUint32(&c.active, 0)
	}()
}

// RecentViolations returns the latest violations.
func (c *Checkpoint) RecentViolations(n int) []Violation {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := len(c.violations)
	if n > total {
		n = total
	}
	result := make([]Violation, n)
	for i := 0; i < n; i++ {
		result[i] = c.violations[total-1-i]
	}
	return result
}

// Stats returns checkpoint statistics.
func (c *Checkpoint) Stats() CheckpointStats {
	return CheckpointStats{
		TotalChecks:  atomic.LoadInt64(&c.stats.TotalChecks),
		PassCount:    atomic.LoadInt64(&c.stats.PassCount),
		FailCount:    atomic.LoadInt64(&c.stats.FailCount),
		WindowsRun:   atomic.LoadInt64(&c.stats.WindowsRun),
		LastWindowAt: c.stats.LastWindowAt,
	}
}

// ClearViolations resets the violation buffer.
func (c *Checkpoint) ClearViolations() {
	c.mu.Lock()
	c.violations = c.violations[:0]
	c.mu.Unlock()
}
