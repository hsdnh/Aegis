package tracecollector

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/hsdnh/ai-ops-agent/sdk/probe"
)

// TraceReceiver listens for trace data from instrumented applications.
type TraceReceiver struct {
	listenAddr string
	conn       net.PacketConn
	mu         sync.RWMutex
	latest     *probe.WindowSnapshot // most recent window data
	history    []AnalyzedWindow      // last N analyzed windows
	funcTable  map[uint32]probe.FuncEntry
	stopCh     chan struct{}
}

// AnalyzedWindow is a post-processed trace window with computed metrics.
type AnalyzedWindow struct {
	Snapshot   probe.WindowSnapshot `json:"snapshot"`
	HotSpots   []HotSpot           `json:"hot_spots"`
	CallTree   []CallNode          `json:"call_tree"`
	Anomalies  []TraceAnomaly      `json:"anomalies"`
	ReceivedAt time.Time           `json:"received_at"`
}

// HotSpot identifies a function consuming disproportionate time.
type HotSpot struct {
	FuncID    uint32        `json:"func_id"`
	FuncName  string        `json:"func_name"`
	TotalTime time.Duration `json:"total_time"`
	CallCount int           `json:"call_count"`
	AvgTime   time.Duration `json:"avg_time"`
	PctOfTotal float64      `json:"pct_of_total"`
}

// CallNode represents one node in a reconstructed call tree.
type CallNode struct {
	FuncID   uint32     `json:"func_id"`
	FuncName string     `json:"func_name"`
	Duration time.Duration `json:"duration"`
	Children []CallNode `json:"children,omitempty"`
}

// TraceAnomaly identifies suspicious patterns in the trace data.
type TraceAnomaly struct {
	Type        string `json:"type"` // "hot_function", "goroutine_leak", "mutex_contention", "gc_pressure"
	Description string `json:"description"`
	Severity    string `json:"severity"`
	FuncName    string `json:"func_name,omitempty"`
}

func NewTraceReceiver(listenAddr string) *TraceReceiver {
	if listenAddr == "" {
		listenAddr = ":19876"
	}
	return &TraceReceiver{
		listenAddr: listenAddr,
		funcTable:  make(map[uint32]probe.FuncEntry),
		stopCh:     make(chan struct{}),
	}
}

// Start begins listening for trace data from instrumented processes.
func (r *TraceReceiver) Start() error {
	conn, err := net.ListenPacket("udp", r.listenAddr)
	if err != nil {
		return err
	}
	r.conn = conn
	log.Printf("[trace-receiver] Listening on %s", r.listenAddr)

	go r.receiveLoop()
	return nil
}

// Stop shuts down the receiver.
func (r *TraceReceiver) Stop() {
	close(r.stopCh)
	if r.conn != nil {
		r.conn.Close()
	}
}

// LatestWindow returns the most recently received trace window.
func (r *TraceReceiver) LatestWindow() *AnalyzedWindow {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.history) == 0 {
		return nil
	}
	return &r.history[len(r.history)-1]
}

// TriggerWindow sends a command to the instrumented process to start tracing.
func (r *TraceReceiver) TriggerWindow(targetAddr string, duration time.Duration, trigger string) error {
	cmd := probe.ControlCommand{
		Action:   "start_window",
		Duration: duration,
		Trigger:  trigger,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("udp", targetAddr, 1*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(data)
	return err
}

func (r *TraceReceiver) receiveLoop() {
	buf := make([]byte, 65536)
	for {
		select {
		case <-r.stopCh:
			return
		default:
		}

		r.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := r.conn.ReadFrom(buf)
		if err != nil {
			continue
		}

		var snapshot probe.WindowSnapshot
		if err := json.Unmarshal(buf[:n], &snapshot); err != nil {
			log.Printf("[trace-receiver] Bad data: %v", err)
			continue
		}

		analyzed := r.analyze(snapshot)

		r.mu.Lock()
		r.latest = &snapshot
		r.history = append(r.history, analyzed)
		if len(r.history) > 100 {
			r.history = r.history[len(r.history)-100:]
		}
		r.mu.Unlock()

		log.Printf("[trace-receiver] Window %d: %d spans, %d goroutines, %d hot spots, %d anomalies",
			snapshot.WindowID, snapshot.SpanCount, snapshot.Goroutines,
			len(analyzed.HotSpots), len(analyzed.Anomalies))
	}
}

func (r *TraceReceiver) analyze(snapshot probe.WindowSnapshot) AnalyzedWindow {
	aw := AnalyzedWindow{
		Snapshot:   snapshot,
		ReceivedAt: time.Now(),
	}

	// Deserialize spans
	if len(snapshot.Spans) == 0 {
		return aw
	}

	spans := make([]probe.Span, snapshot.SpanCount)
	for i := 0; i < snapshot.SpanCount && i*36+36 <= len(snapshot.Spans); i++ {
		spans[i] = probe.DeserializeSpan(snapshot.Spans[i*36 : (i+1)*36])
	}

	// Compute hot spots
	type funcStats struct {
		totalNs int64
		count   int
	}
	stats := make(map[uint32]*funcStats)

	for _, s := range spans {
		if s.EndNano > 0 && s.StartNano > 0 {
			dur := s.EndNano - s.StartNano
			st, ok := stats[s.FuncID]
			if !ok {
				st = &funcStats{}
				stats[s.FuncID] = st
			}
			st.totalNs += dur
			st.count++
		}
	}

	var totalNs int64
	for _, st := range stats {
		totalNs += st.totalNs
	}

	for funcID, st := range stats {
		if st.count == 0 {
			continue
		}
		pct := float64(st.totalNs) / float64(max(totalNs, 1)) * 100
		aw.HotSpots = append(aw.HotSpots, HotSpot{
			FuncID:     funcID,
			FuncName:   r.funcName(funcID),
			TotalTime:  time.Duration(st.totalNs),
			CallCount:  st.count,
			AvgTime:    time.Duration(st.totalNs / int64(st.count)),
			PctOfTotal: pct,
		})
	}

	// Detect anomalies
	aw.Anomalies = r.detectAnomalies(snapshot, aw.HotSpots)

	return aw
}

func (r *TraceReceiver) detectAnomalies(snapshot probe.WindowSnapshot, hotSpots []HotSpot) []TraceAnomaly {
	var anomalies []TraceAnomaly

	// 1. Any function taking >80% of total time
	for _, hs := range hotSpots {
		if hs.PctOfTotal > 80 {
			anomalies = append(anomalies, TraceAnomaly{
				Type:        "hot_function",
				Description: fmt.Sprintf("%s consumes %.0f%% of total time (%d calls, avg %v)",
					hs.FuncName, hs.PctOfTotal, hs.CallCount, hs.AvgTime),
				Severity: "critical",
				FuncName: hs.FuncName,
			})
		} else if hs.PctOfTotal > 50 {
			anomalies = append(anomalies, TraceAnomaly{
				Type:        "hot_function",
				Description: fmt.Sprintf("%s consumes %.0f%% of total time (%d calls, avg %v)",
					hs.FuncName, hs.PctOfTotal, hs.CallCount, hs.AvgTime),
				Severity: "warning",
				FuncName: hs.FuncName,
			})
		}
	}

	// 2. Goroutine count anomaly (>1000 suggests leak)
	if snapshot.Goroutines > 1000 {
		anomalies = append(anomalies, TraceAnomaly{
			Type:        "goroutine_leak",
			Description: fmt.Sprintf("Goroutine count: %d (>1000 suggests possible leak)", snapshot.Goroutines),
			Severity:    "warning",
		})
	}

	// 3. GC pressure
	if snapshot.GCPauseNs > 10_000_000 { // >10ms pause
		anomalies = append(anomalies, TraceAnomaly{
			Type:        "gc_pressure",
			Description: fmt.Sprintf("GC pause: %v (>10ms indicates memory pressure)", time.Duration(snapshot.GCPauseNs)),
			Severity:    "warning",
		})
	}

	// 4. High memory usage
	if snapshot.HeapAllocMB > 500 {
		anomalies = append(anomalies, TraceAnomaly{
			Type:        "high_memory",
			Description: fmt.Sprintf("Heap: %dMB (high for typical Go service)", snapshot.HeapAllocMB),
			Severity:    "warning",
		})
	}

	return anomalies
}

func (r *TraceReceiver) funcName(id uint32) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.funcTable[id]; ok {
		return entry.Name
	}
	return fmt.Sprintf("func_%d", id)
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
