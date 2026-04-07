package types

import "time"

// Severity represents alert severity levels.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityCritical
	SeverityFatal
)

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "INFO"
	case SeverityWarning:
		return "WARNING"
	case SeverityCritical:
		return "CRITICAL"
	case SeverityFatal:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// Metric represents a single collected metric.
type Metric struct {
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
	Source    string            `json:"source"`
}

// LogEntry represents a collected log line.
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Source    string    `json:"source"`
	Fields    map[string]string `json:"fields,omitempty"`
}

// CollectResult holds everything a collector gathers in one cycle.
type CollectResult struct {
	CollectorName string     `json:"collector_name"`
	Metrics       []Metric   `json:"metrics"`
	Logs          []LogEntry `json:"logs"`
	Errors        []string   `json:"errors,omitempty"`
	CollectedAt   time.Time  `json:"collected_at"`
}

// RuleResult represents the outcome of a rule evaluation.
type RuleResult struct {
	RuleName    string   `json:"rule_name"`
	Triggered   bool     `json:"triggered"`
	Severity    Severity `json:"severity"`
	Message     string   `json:"message"`
	MetricName  string   `json:"metric_name"`
	MetricValue float64  `json:"metric_value"`
	Threshold   float64  `json:"threshold"`
}

// Alert is what gets sent to notification channels.
type Alert struct {
	ID        string            `json:"id"`
	Severity  Severity          `json:"severity"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Source    string            `json:"source"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// AIAnalysis holds results from AI-powered analysis.
type AIAnalysis struct {
	Summary       string   `json:"summary"`
	RootCause     string   `json:"root_cause,omitempty"`
	Suggestions   []string `json:"suggestions,omitempty"`
	Confidence    float64  `json:"confidence"` // 0.0 - 1.0
	AffectedAreas []string `json:"affected_areas,omitempty"`
}

// --- Issue Lifecycle Model ---

// IssueStatus represents the lifecycle state of a tracked issue.
type IssueStatus string

const (
	IssueDetecting  IssueStatus = "DETECTING"  // Anomaly seen once, waiting for confirmation
	IssueOpen       IssueStatus = "OPEN"       // Confirmed anomaly, user notified
	IssueAcked      IssueStatus = "ACKED"      // User acknowledged, muted temporarily
	IssueMonitoring IssueStatus = "MONITORING"  // Fix deployed, verifying recovery
	IssueImproving  IssueStatus = "IMPROVING"   // Metrics improving but not fully recovered
	IssueResolved   IssueStatus = "RESOLVED"    // All evidence shows recovery
	IssueClosed     IssueStatus = "CLOSED"      // Stable for N cycles, auto-closed
	IssueFlapping   IssueStatus = "FLAPPING"    // Oscillating open/resolved, suppressed
	IssueDataGap    IssueStatus = "DATA_GAP"    // Cannot assess — collector failed
)

// Issue tracks an anomaly through its full lifecycle.
type Issue struct {
	ID          string      `json:"id"`
	Fingerprint string      `json:"fingerprint"` // service + symptom + labels + entity
	Status      IssueStatus `json:"status"`
	Severity    Severity    `json:"severity"`
	Title       string      `json:"title"`
	Summary     string      `json:"summary"`

	// Evidence chain
	Evidence    []Evidence  `json:"evidence"`
	RootCause   string      `json:"root_cause,omitempty"`
	Suggestions []string    `json:"suggestions,omitempty"`
	CodeRefs    []string    `json:"code_refs,omitempty"` // e.g. "proxy_order.go:127"
	Confidence  float64     `json:"confidence"`          // 0.0 - 1.0

	// Dependency / grouping
	ParentID    string      `json:"parent_id,omitempty"` // links to root-cause incident
	ChildIDs    []string    `json:"child_ids,omitempty"` // suppressed downstream issues
	DependsOn   []string    `json:"depends_on,omitempty"` // component dependencies

	// Lifecycle tracking
	FirstSeenAt time.Time   `json:"first_seen_at"`
	LastSeenAt  time.Time   `json:"last_seen_at"`
	ResolvedAt  *time.Time  `json:"resolved_at,omitempty"`
	ClosedAt    *time.Time  `json:"closed_at,omitempty"`
	UpdatedAt   time.Time   `json:"updated_at"`

	// Anti-flapping
	ConsecutiveBad  int `json:"consecutive_bad"`  // cycles anomaly persisted
	ConsecutiveGood int `json:"consecutive_good"` // cycles anomaly absent
	ReopenCount     int `json:"reopen_count"`     // times reopened within 24h
	FlapScore       int `json:"flap_score"`       // higher = more unstable

	// Notification state
	LastNotifiedAt *time.Time `json:"last_notified_at,omitempty"`
	MutedUntil     *time.Time `json:"muted_until,omitempty"`

	// History
	History []IssueEvent `json:"history"`
}

// Evidence represents a piece of proof for an issue.
type Evidence struct {
	Timestamp   time.Time `json:"timestamp"`
	Type        string    `json:"type"` // "metric", "log", "api_response", "config_change"
	Description string    `json:"description"`
	Value       string    `json:"value"`
	Source      string    `json:"source"`
}

// IssueEvent records a status change in the issue history.
type IssueEvent struct {
	Timestamp time.Time   `json:"timestamp"`
	FromStatus IssueStatus `json:"from_status"`
	ToStatus   IssueStatus `json:"to_status"`
	Reason     string      `json:"reason"`
	CycleID    string      `json:"cycle_id"`
}

// --- Snapshot ---

// SnapshotHealth indicates how complete/trustworthy a snapshot is.
type SnapshotHealth struct {
	TotalCollectors   int     `json:"total_collectors"`
	SuccessCollectors int     `json:"success_collectors"`
	FailedCollectors  int     `json:"failed_collectors"`
	Completeness      float64 `json:"completeness"`      // 0.0 - 1.0
	Freshness         float64 `json:"freshness"`          // 1.0 = all data fresh, 0.0 = all stale
	Trustworthy       bool    `json:"trustworthy"`        // false if completeness < threshold
}

// Snapshot captures the full state of one monitoring cycle.
type Snapshot struct {
	ProjectName string          `json:"project_name"`
	CycleID     string          `json:"cycle_id"`
	Results     []CollectResult `json:"results"`
	RuleResults []RuleResult    `json:"rule_results"`
	Analysis    *AIAnalysis     `json:"analysis,omitempty"`
	Alerts      []Alert         `json:"alerts"`
	Health      SnapshotHealth  `json:"health"`
	Timestamp   time.Time       `json:"timestamp"`
}

// --- Agent Self-Health ---

// AgentHealth captures the agent's own operational status.
type AgentHealth struct {
	NetworkOK    bool      `json:"network_ok"`    // can reach external gateway
	DiskOK       bool      `json:"disk_ok"`       // enough disk space
	LastCycleOK  bool      `json:"last_cycle_ok"` // previous cycle completed
	StartedAt    time.Time `json:"started_at"`
	UptimeSeconds int64    `json:"uptime_seconds"`
}
