// Package cluster implements multi-node distributed monitoring.
//
// Architecture:
//   - Standalone: single node, all features (default, backward compatible)
//   - Worker: full local monitoring + reports results to master
//   - Master: aggregates all worker data + cross-node AI analysis + unified dashboard
//
// Workers send compressed NodeReport to master every cycle.
// Reports contain analysis results, not raw data — bandwidth efficient.
package cluster

import (
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

// Mode defines how this agent instance operates.
type Mode string

const (
	ModeStandalone Mode = "standalone" // single node, all features
	ModeWorker     Mode = "worker"     // full local monitoring + report to master
	ModeMaster     Mode = "master"     // aggregate workers + cross-node analysis + dashboard
)

// NodeInfo identifies one agent instance in the cluster.
type NodeInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	IP        string    `json:"ip"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	Mode      Mode      `json:"mode"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// NodeReport is what a worker sends to the master after each monitoring cycle.
// Contains analysis results, not raw data — keeps bandwidth low.
type NodeReport struct {
	Node        NodeInfo              `json:"node"`
	CycleID     string                `json:"cycle_id"`
	Timestamp   time.Time             `json:"timestamp"`
	Health      types.SnapshotHealth  `json:"health"`
	AgentHealth types.AgentHealth     `json:"agent_health"`

	// Summarized metrics (key values only, not full series)
	MetricSummary []MetricSummary     `json:"metric_summary"`

	// Triggered rules
	TriggeredRules []types.RuleResult `json:"triggered_rules"`

	// Open issues on this node
	Issues []*types.Issue            `json:"issues"`

	// Error log summary (count + top 10)
	ErrorLogCount int                `json:"error_log_count"`
	TopErrors     []string           `json:"top_errors"`

	// Trace summary (if available)
	TraceHotSpots []TraceHotSpot     `json:"trace_hot_spots,omitempty"`

	// Checkpoint violations
	Violations []string              `json:"violations,omitempty"`

	// System resources
	CPUPercent   float64             `json:"cpu_percent"`
	MemoryMB     int                 `json:"memory_mb"`
	DiskPercent  float64             `json:"disk_percent"`
	Goroutines   int                 `json:"goroutines"`
}

// MetricSummary is a condensed metric for cross-node comparison.
type MetricSummary struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Status string  `json:"status"` // "ok", "warning", "critical"
}

// TraceHotSpot is a condensed trace hot spot.
type TraceHotSpot struct {
	FuncName   string  `json:"func_name"`
	PctOfTotal float64 `json:"pct_of_total"`
	AvgMs      float64 `json:"avg_ms"`
}

// NewNodeInfo creates info for the current machine.
func NewNodeInfo(name, version string, mode Mode) NodeInfo {
	hostname, _ := os.Hostname()
	return NodeInfo{
		ID:        hostname + "-" + name,
		Name:      name,
		Host:      hostname,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Mode:      mode,
		Version:   version,
		StartedAt: time.Now(),
		LastSeen:  time.Now(),
	}
}

// NodeRegistry tracks all known nodes in the cluster.
type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeState // keyed by node ID
}

// NodeState holds the latest data from a node.
type NodeState struct {
	Info       NodeInfo    `json:"info"`
	LastReport *NodeReport `json:"last_report"`
	Online     bool        `json:"online"`
	LastSeen   time.Time   `json:"last_seen"`
}

func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{
		nodes: make(map[string]*NodeState),
	}
}

// Update processes an incoming report from a worker node.
func (nr *NodeRegistry) Update(report NodeReport) {
	nr.mu.Lock()
	defer nr.mu.Unlock()

	state, ok := nr.nodes[report.Node.ID]
	if !ok {
		state = &NodeState{Info: report.Node}
		nr.nodes[report.Node.ID] = state
	}

	state.Info = report.Node
	state.Info.LastSeen = time.Now()
	state.LastReport = &report
	state.Online = true
	state.LastSeen = time.Now()
}

// AllNodes returns all known nodes.
func (nr *NodeRegistry) AllNodes() []NodeState {
	nr.mu.RLock()
	defer nr.mu.RUnlock()

	// Mark nodes offline if no report in 5 minutes
	cutoff := time.Now().Add(-5 * time.Minute)
	var nodes []NodeState
	for _, n := range nr.nodes {
		if n.LastSeen.Before(cutoff) {
			n.Online = false
		}
		nodes = append(nodes, *n)
	}
	return nodes
}

// OnlineCount returns how many nodes are currently online.
func (nr *NodeRegistry) OnlineCount() int {
	nr.mu.RLock()
	defer nr.mu.RUnlock()
	count := 0
	cutoff := time.Now().Add(-5 * time.Minute)
	for _, n := range nr.nodes {
		if n.LastSeen.After(cutoff) {
			count++
		}
	}
	return count
}

// GetNode returns a specific node's state.
func (nr *NodeRegistry) GetNode(nodeID string) *NodeState {
	nr.mu.RLock()
	defer nr.mu.RUnlock()
	if n, ok := nr.nodes[nodeID]; ok {
		return n
	}
	return nil
}
