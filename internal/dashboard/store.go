package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hsdnh/ai-ops-agent/internal/ai"
	"github.com/hsdnh/ai-ops-agent/internal/tracecollector"
	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

// MetricKey generates a composite key from metric name + labels.
// e.g., "http.latency_ms{url=/v1/order}" vs "http.latency_ms{url=/v1/search}"
func MetricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	var parts []string
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(parts)
	return name + "{" + strings.Join(parts, ",") + "}"
}

// Store holds all data the dashboard needs. Thread-safe.
// The agent pushes data here after each cycle; the dashboard reads it.
type Store struct {
	mu sync.RWMutex

	// Current state
	projectName string
	agentHealth *types.AgentHealth
	latestSnap  *types.Snapshot

	// History (ring buffer style, fixed capacity)
	snapshots  []types.Snapshot  // last N snapshots
	maxSnaps   int

	// Issues
	openIssues   []*types.Issue
	closedIssues []*types.Issue

	// AI analysis
	latestAnalysis *ai.AnalysisResult
	analysisHistory []AnalysisRecord

	// Trace
	latestTrace *tracecollector.AnalyzedWindow

	// Metrics time series (for charts)
	metricSeries map[string][]MetricPoint // metric name → time series
	maxPoints    int
}

// MetricPoint is a single data point for charting.
type MetricPoint struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

// AnalysisRecord stores one AI analysis for history.
type AnalysisRecord struct {
	Timestamp time.Time          `json:"timestamp"`
	Result    *ai.AnalysisResult `json:"result"`
	CycleID   string             `json:"cycle_id"`
}

func NewStore(projectName string) *Store {
	return &Store{
		projectName:  projectName,
		maxSnaps:     100,
		maxPoints:    288, // 24h at 5min intervals
		metricSeries: make(map[string][]MetricPoint),
	}
}

// PushSnapshot records a new monitoring cycle result.
func (s *Store) PushSnapshot(snap *types.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.latestSnap = snap
	s.snapshots = append(s.snapshots, *snap)
	if len(s.snapshots) > s.maxSnaps {
		s.snapshots = s.snapshots[len(s.snapshots)-s.maxSnaps:]
	}

	// Update metric series — keyed by name+labels for per-entity isolation
	for _, r := range snap.Results {
		for _, m := range r.Metrics {
			key := MetricKey(m.Name, m.Labels)
			pts := s.metricSeries[key]
			pts = append(pts, MetricPoint{Time: m.Timestamp, Value: m.Value})
			if len(pts) > s.maxPoints {
				pts = pts[len(pts)-s.maxPoints:]
			}
			s.metricSeries[key] = pts
		}
	}
}

func (s *Store) PushAgentHealth(h *types.AgentHealth) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentHealth = h
}

func (s *Store) PushIssues(open, closed []*types.Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openIssues = open
	s.closedIssues = closed
}

func (s *Store) PushAnalysis(result *ai.AnalysisResult, cycleID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latestAnalysis = result
	s.analysisHistory = append(s.analysisHistory, AnalysisRecord{
		Timestamp: time.Now(), Result: result, CycleID: cycleID,
	})
	if len(s.analysisHistory) > 50 {
		s.analysisHistory = s.analysisHistory[len(s.analysisHistory)-50:]
	}
}

func (s *Store) PushTrace(trace *tracecollector.AnalyzedWindow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latestTrace = trace
}

// --- Read methods (for API handlers) ---

// Overview returns the top-level dashboard data.
type Overview struct {
	ProjectName    string              `json:"project_name"`
	AgentHealth    *types.AgentHealth  `json:"agent_health"`
	SnapshotHealth *types.SnapshotHealth `json:"snapshot_health"`
	CycleID        string              `json:"cycle_id"`
	Timestamp      time.Time           `json:"timestamp"`
	TotalMetrics   int                 `json:"total_metrics"`
	TriggeredRules int                 `json:"triggered_rules"`
	OpenIssueCount int                 `json:"open_issue_count"`
	AlertCount     int                 `json:"alert_count"`
	HasAIAnalysis  bool                `json:"has_ai_analysis"`
	HasTraceData   bool                `json:"has_trace_data"`
	UptimeSeconds  int64               `json:"uptime_seconds"`
}

func (s *Store) GetOverview() Overview {
	s.mu.RLock()
	defer s.mu.RUnlock()

	o := Overview{ProjectName: s.projectName}
	if s.agentHealth != nil {
		o.AgentHealth = s.agentHealth
		o.UptimeSeconds = s.agentHealth.UptimeSeconds
	}
	if s.latestSnap != nil {
		o.SnapshotHealth = &s.latestSnap.Health
		o.CycleID = s.latestSnap.CycleID
		o.Timestamp = s.latestSnap.Timestamp
		o.AlertCount = len(s.latestSnap.Alerts)

		for _, r := range s.latestSnap.Results {
			o.TotalMetrics += len(r.Metrics)
		}
		for _, rr := range s.latestSnap.RuleResults {
			if rr.Triggered {
				o.TriggeredRules++
			}
		}
	}
	o.OpenIssueCount = len(s.openIssues)
	o.HasAIAnalysis = s.latestAnalysis != nil
	o.HasTraceData = s.latestTrace != nil
	return o
}

func (s *Store) GetMetrics() []types.Metric {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.latestSnap == nil {
		return nil
	}
	var all []types.Metric
	for _, r := range s.latestSnap.Results {
		all = append(all, r.Metrics...)
	}
	return all
}

func (s *Store) GetMetricSeries(name string) []MetricPoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metricSeries[name]
}

func (s *Store) GetAllMetricNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var names []string
	for name := range s.metricSeries {
		names = append(names, name)
	}
	return names
}

func (s *Store) GetIssues() []*types.Issue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.openIssues
}

func (s *Store) GetClosedIssues() []*types.Issue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closedIssues
}

func (s *Store) GetRuleResults() []types.RuleResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.latestSnap == nil {
		return nil
	}
	return s.latestSnap.RuleResults
}

func (s *Store) GetLatestAnalysis() *ai.AnalysisResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latestAnalysis
}

func (s *Store) GetLatestTrace() *tracecollector.AnalyzedWindow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latestTrace
}

func (s *Store) GetLogs() []types.LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.latestSnap == nil {
		return nil
	}
	var logs []types.LogEntry
	for _, r := range s.latestSnap.Results {
		logs = append(logs, r.Logs...)
	}
	return logs
}
