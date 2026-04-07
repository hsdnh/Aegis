package probe

import (
	"encoding/json"
	"net"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Global probe instance — one per process, like a virus payload.
var Global = &Probe{}

// Probe is the core tracing controller embedded in the target process.
// Design principles (borrowed from virus engineering):
//   - 4MB fixed memory, never grows
//   - atomic switch: 1ns check when off, ~50ns record when on
//   - fire-and-forget reporting: never blocks business code
//   - any panic in probe code is silently swallowed
type Probe struct {
	active    uint32 // atomic: 0=dormant, 1=sampling, 2=deep
	ring      RingBuffer
	funcs     []FuncEntry // registered at startup, immutable after
	funcsMu   sync.Mutex
	agentAddr string
	windowID  uint64

	// goroutine-local depth tracking (per-G, lock-free)
	depths sync.Map // goroutineID → *uint32

	// scheduling
	stopCh    chan struct{}
	running   uint32
}

// Init sets up the probe. Called once from autotrace init().
func (p *Probe) Init(agentAddr string) {
	if agentAddr == "" {
		agentAddr = "127.0.0.1:19876"
	}
	p.agentAddr = agentAddr
	p.stopCh = make(chan struct{})
}

// RegisterFunc adds a function to the name table. Returns its ID.
// Called once per instrumented function at package init time.
func (p *Probe) RegisterFunc(name, file string, line int32, pkg string, priority byte) uint32 {
	p.funcsMu.Lock()
	defer p.funcsMu.Unlock()
	id := uint32(len(p.funcs))
	p.funcs = append(p.funcs, FuncEntry{
		ID: id, Name: name, File: file, Line: line, Package: pkg, Priority: priority,
	})
	return id
}

// Active returns true if tracing is currently enabled.
// This is THE hot path — called at every instrumented function entry.
// Must be as fast as possible: single atomic load ≈ 1ns.
//
//go:nosplit
func (p *Probe) Active() bool {
	return atomic.LoadUint32(&p.active) > 0
}

// Enter starts tracing a function call. Only called when Active() == true.
// Returns a token that must be passed to Exit via defer.
func (p *Probe) Enter(funcID uint32) uint64 {
	gid := goroutineID()
	depth := p.getDepth(gid)
	atomic.AddUint32(depth, 1)

	now := monotimeNano()
	// Encode start info into a uint64 token: high 32 = funcID, low 32 = ring position
	token := (uint64(funcID) << 32) | uint64(now&0xFFFFFFFF)

	s := Span{
		FuncID:      funcID,
		GoroutineID: gid,
		StartNano:   now,
		Depth:       uint16(atomic.LoadUint32(depth)),
	}
	p.ring.Write(s)
	return token
}

// Exit completes a function trace. Called via defer.
func (p *Probe) Exit(token uint64) {
	funcID := uint32(token >> 32)
	gid := goroutineID()
	depth := p.getDepth(gid)
	now := monotimeNano()

	s := Span{
		FuncID:      funcID,
		GoroutineID: gid,
		EndNano:     now,
		Depth:       uint16(atomic.LoadUint32(depth)),
	}
	p.ring.Write(s)

	if d := atomic.LoadUint32(depth); d > 0 {
		atomic.AddUint32(depth, ^uint32(0)) // decrement
	}
}

// --- Window Control ---

// StartWindow activates tracing for the given duration.
// No-op if a window is already active (prevents concurrent windows).
func (p *Probe) StartWindow(duration time.Duration, trigger string) {
	if !atomic.CompareAndSwapUint32(&p.active, 0, 1) {
		return // another window is already active
	}
	wid := atomic.AddUint64(&p.windowID, 1)

	go func() {
		defer func() { recover() }() // never crash the host
		time.Sleep(duration)
		atomic.StoreUint32(&p.active, 0)
		p.flushWindow(wid, duration, trigger)
	}()
}

// StartDeepWindow activates deep tracing (all functions, with args).
func (p *Probe) StartDeepWindow(duration time.Duration) {
	if !atomic.CompareAndSwapUint32(&p.active, 0, 2) {
		return // another window is already active
	}
	wid := atomic.AddUint64(&p.windowID, 1)

	go func() {
		defer func() { recover() }()
		time.Sleep(duration)
		atomic.StoreUint32(&p.active, 0)
		p.flushWindow(wid, duration, "deep")
	}()
}

// --- Background Scheduler ---

// StartScheduler runs periodic trace windows. Like a virus heartbeat.
func (p *Probe) StartScheduler(interval, windowDuration time.Duration) {
	if !atomic.CompareAndSwapUint32(&p.running, 0, 1) {
		return // already running
	}
	go func() {
		defer func() { recover() }()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if atomic.LoadUint32(&p.active) == 0 {
					p.StartWindow(windowDuration, "scheduled")
				}
			case <-p.stopCh:
				return
			}
		}
	}()
}

// Stop shuts down the scheduler.
func (p *Probe) Stop() {
	atomic.StoreUint32(&p.active, 0)
	if atomic.CompareAndSwapUint32(&p.running, 1, 0) {
		close(p.stopCh)
	}
}

// --- Internal ---

func (p *Probe) flushWindow(windowID uint64, duration time.Duration, trigger string) {
	spans, count := p.ring.Drain()
	if count == 0 {
		return
	}

	// Capture runtime stats
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	var gcStats debug.GCStats
	debug.ReadGCStats(&gcStats)

	var gcPause int64
	if len(gcStats.Pause) > 0 {
		gcPause = int64(gcStats.Pause[0])
	}

	snapshot := WindowSnapshot{
		WindowID:    windowID,
		StartTime:   time.Now().Add(-duration),
		Duration:    duration,
		Trigger:     trigger,
		SpanCount:   count,
		Goroutines:  runtime.NumGoroutine(),
		HeapAllocMB: int(memStats.HeapAlloc / 1024 / 1024),
		GCPauseNs:   gcPause,
	}

	// Serialize spans to bytes
	buf := make([]byte, count*36) // 36 bytes per serialized span
	for i, s := range spans {
		SerializeSpan(&s, buf[i*36:(i+1)*36])
	}
	snapshot.Spans = buf

	// Fire and forget — never block, never retry
	p.sendToAgent(snapshot)
}

func (p *Probe) sendToAgent(snapshot WindowSnapshot) {
	defer func() { recover() }() // swallow any error

	conn, err := net.DialTimeout("udp", p.agentAddr, 100*time.Millisecond)
	if err != nil {
		return // agent unreachable — silently drop
	}
	defer conn.Close()

	data, err := json.Marshal(snapshot)
	if err != nil {
		return
	}

	// UDP max ~65KB. If data exceeds, send header only.
	if len(data) > 60000 {
		snapshot.Spans = nil
		data, _ = json.Marshal(snapshot)
	}

	conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	conn.Write(data)
}

func (p *Probe) getDepth(gid uint64) *uint32 {
	if v, ok := p.depths.Load(gid); ok {
		return v.(*uint32)
	}
	d := new(uint32)
	actual, _ := p.depths.LoadOrStore(gid, d)
	return actual.(*uint32)
}

// gidCounter is a fast goroutine-local ID generator.
// runtime.Stack-based goroutineID() costs ~1μs which is too slow for hot path.
// Instead we use a per-goroutine counter via sync.Map keyed by a cheap stack pointer hint.
// This doesn't give the real goroutine ID but gives a unique per-G identifier which is all we need.
var gidCounter uint64

// goroutineID returns a unique identifier for the current goroutine.
// Uses atomic counter + goroutine-local storage for O(1) performance.
func goroutineID() uint64 {
	// Fast path: check TLS-like storage via sync.Map
	// We use the address of a stack-local variable as a cheap goroutine hint.
	// Different goroutines will have different stack addresses.
	var stackLocal int
	key := uintptr(unsafe.Pointer(&stackLocal)) >> 12 // page-aligned hint

	if v, ok := gidStore.Load(key); ok {
		return v.(uint64)
	}
	// Slow path: allocate a new ID (happens once per goroutine)
	id := atomic.AddUint64(&gidCounter, 1)
	gidStore.Store(key, id)
	return id
}

var gidStore sync.Map

// monotimeNano returns monotonic clock nanoseconds.
func monotimeNano() int64 {
	return time.Now().UnixNano() // TODO: use runtime.nanotime for true monotonic
}
