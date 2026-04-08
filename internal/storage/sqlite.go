// Package storage provides SQLite-based persistence for issues, metrics, and analysis history.
// Data survives agent restarts. Uses WAL mode for concurrent read/write.
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

// DB wraps a SQLite connection with application-specific operations.
type DB struct {
	db *sql.DB
}

// Open creates or opens a SQLite database at the given path.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite doesn't benefit from multiple writers

	s := &DB{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database connection.
func (s *DB) Close() error {
	return s.db.Close()
}

func (s *DB) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS issues (
			id TEXT PRIMARY KEY,
			fingerprint TEXT NOT NULL,
			status TEXT NOT NULL,
			severity INTEGER NOT NULL,
			title TEXT NOT NULL,
			summary TEXT,
			root_cause TEXT,
			suggestions TEXT,  -- JSON array
			code_refs TEXT,    -- JSON array
			confidence REAL,
			parent_id TEXT,
			first_seen_at DATETIME NOT NULL,
			last_seen_at DATETIME NOT NULL,
			resolved_at DATETIME,
			closed_at DATETIME,
			updated_at DATETIME NOT NULL,
			consecutive_bad INTEGER DEFAULT 0,
			consecutive_good INTEGER DEFAULT 0,
			reopen_count INTEGER DEFAULT 0,
			flap_score INTEGER DEFAULT 0,
			evidence TEXT,     -- JSON
			history TEXT       -- JSON
		);
		CREATE INDEX IF NOT EXISTS idx_issues_status ON issues(status);
		CREATE INDEX IF NOT EXISTS idx_issues_fingerprint ON issues(fingerprint);

		CREATE TABLE IF NOT EXISTS metrics_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			value REAL NOT NULL,
			source TEXT,
			labels TEXT,       -- JSON
			timestamp DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_metrics_name_ts ON metrics_history(name, timestamp);

		CREATE TABLE IF NOT EXISTS analysis_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			cycle_id TEXT NOT NULL,
			health_summary TEXT,
			confidence REAL,
			anomalies TEXT,    -- JSON
			input_tokens INTEGER,
			output_tokens INTEGER,
			rounds INTEGER,
			created_at DATETIME NOT NULL
		);

		CREATE TABLE IF NOT EXISTS baselines (
			metric_name TEXT PRIMARY KEY,
			mean REAL NOT NULL,
			std_dev REAL NOT NULL,
			min_val REAL NOT NULL,
			max_val REAL NOT NULL,
			sample_count INTEGER NOT NULL,
			updated_at DATETIME NOT NULL
		);
	`)
	return err
}

// --- Issues ---

// SaveIssue upserts an issue.
func (s *DB) SaveIssue(iss *types.Issue) error {
	suggestions, _ := json.Marshal(iss.Suggestions)
	codeRefs, _ := json.Marshal(iss.CodeRefs)
	evidence, _ := json.Marshal(iss.Evidence)
	history, _ := json.Marshal(iss.History)

	_, err := s.db.Exec(`
		INSERT INTO issues (id, fingerprint, status, severity, title, summary, root_cause,
			suggestions, code_refs, confidence, parent_id, first_seen_at, last_seen_at,
			resolved_at, closed_at, updated_at, consecutive_bad, consecutive_good,
			reopen_count, flap_score, evidence, history)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status, severity=excluded.severity, summary=excluded.summary,
			root_cause=excluded.root_cause, suggestions=excluded.suggestions,
			code_refs=excluded.code_refs, confidence=excluded.confidence,
			last_seen_at=excluded.last_seen_at, resolved_at=excluded.resolved_at,
			closed_at=excluded.closed_at, updated_at=excluded.updated_at,
			consecutive_bad=excluded.consecutive_bad, consecutive_good=excluded.consecutive_good,
			reopen_count=excluded.reopen_count, flap_score=excluded.flap_score,
			evidence=excluded.evidence, history=excluded.history`,
		iss.ID, iss.Fingerprint, string(iss.Status), int(iss.Severity), iss.Title,
		iss.Summary, iss.RootCause, string(suggestions), string(codeRefs),
		iss.Confidence, iss.ParentID, iss.FirstSeenAt, iss.LastSeenAt,
		iss.ResolvedAt, iss.ClosedAt, iss.UpdatedAt,
		iss.ConsecutiveBad, iss.ConsecutiveGood, iss.ReopenCount, iss.FlapScore,
		string(evidence), string(history),
	)
	return err
}

// LoadOpenIssues returns all non-closed issues.
func (s *DB) LoadOpenIssues() ([]*types.Issue, error) {
	rows, err := s.db.Query(`SELECT id, fingerprint, status, severity, title, summary,
		root_cause, suggestions, code_refs, confidence, parent_id,
		first_seen_at, last_seen_at, resolved_at, closed_at, updated_at,
		consecutive_bad, consecutive_good, reopen_count, flap_score,
		evidence, history
		FROM issues WHERE status != 'CLOSED' ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var issues []*types.Issue
	for rows.Next() {
		iss, err := scanIssue(rows)
		if err != nil {
			continue
		}
		issues = append(issues, iss)
	}
	return issues, nil
}

func scanIssue(rows *sql.Rows) (*types.Issue, error) {
	var iss types.Issue
	var status string
	var severity int
	var suggestions, codeRefs, evidence, history string
	var resolvedAt, closedAt sql.NullTime

	err := rows.Scan(&iss.ID, &iss.Fingerprint, &status, &severity, &iss.Title,
		&iss.Summary, &iss.RootCause, &suggestions, &codeRefs, &iss.Confidence,
		&iss.ParentID, &iss.FirstSeenAt, &iss.LastSeenAt, &resolvedAt, &closedAt,
		&iss.UpdatedAt, &iss.ConsecutiveBad, &iss.ConsecutiveGood,
		&iss.ReopenCount, &iss.FlapScore, &evidence, &history)
	if err != nil {
		return nil, err
	}

	iss.Status = types.IssueStatus(status)
	iss.Severity = types.Severity(severity)
	if resolvedAt.Valid {
		iss.ResolvedAt = &resolvedAt.Time
	}
	if closedAt.Valid {
		iss.ClosedAt = &closedAt.Time
	}
	json.Unmarshal([]byte(suggestions), &iss.Suggestions)
	json.Unmarshal([]byte(codeRefs), &iss.CodeRefs)
	json.Unmarshal([]byte(evidence), &iss.Evidence)
	json.Unmarshal([]byte(history), &iss.History)

	return &iss, nil
}

// --- Metrics ---

// SaveMetrics saves a batch of metrics for historical charting.
func (s *DB) SaveMetrics(metrics []types.Metric) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO metrics_history (name, value, source, labels, timestamp) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, m := range metrics {
		labels, _ := json.Marshal(m.Labels)
		stmt.Exec(m.Name, m.Value, m.Source, string(labels), m.Timestamp)
	}
	return tx.Commit()
}

// QueryMetricSeries returns time series for a metric in the given time range.
func (s *DB) QueryMetricSeries(name string, since time.Time) ([]struct {
	Time  time.Time
	Value float64
}, error) {
	rows, err := s.db.Query(`SELECT timestamp, value FROM metrics_history
		WHERE name = ? AND timestamp > ? ORDER BY timestamp`, name, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []struct {
		Time  time.Time
		Value float64
	}
	for rows.Next() {
		var point struct {
			Time  time.Time
			Value float64
		}
		rows.Scan(&point.Time, &point.Value)
		result = append(result, point)
	}
	return result, nil
}

// CleanOldMetrics removes metrics older than the given duration.
func (s *DB) CleanOldMetrics(olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan)
	_, err := s.db.Exec(`DELETE FROM metrics_history WHERE timestamp < ?`, cutoff)
	return err
}

// --- Baselines ---

// Baseline holds statistical summary for a metric.
type Baseline struct {
	Mean        float64
	StdDev      float64
	Min         float64
	Max         float64
	SampleCount int
	UpdatedAt   time.Time
}

// SaveBaseline stores or updates a metric baseline.
func (s *DB) SaveBaseline(metricName string, bl Baseline) error {
	_, err := s.db.Exec(`INSERT INTO baselines (metric_name, mean, std_dev, min_val, max_val, sample_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(metric_name) DO UPDATE SET
			mean=excluded.mean, std_dev=excluded.std_dev, min_val=excluded.min_val,
			max_val=excluded.max_val, sample_count=excluded.sample_count, updated_at=excluded.updated_at`,
		metricName, bl.Mean, bl.StdDev, bl.Min, bl.Max, bl.SampleCount, bl.UpdatedAt)
	return err
}

// LoadBaselines returns all saved baselines.
func (s *DB) LoadBaselines() (map[string]Baseline, error) {
	rows, err := s.db.Query(`SELECT metric_name, mean, std_dev, min_val, max_val, sample_count, updated_at FROM baselines`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]Baseline)
	for rows.Next() {
		var name string
		var bl Baseline
		rows.Scan(&name, &bl.Mean, &bl.StdDev, &bl.Min, &bl.Max, &bl.SampleCount, &bl.UpdatedAt)
		result[name] = bl
	}
	return result, nil
}

// --- Analysis History ---

// SaveAnalysis stores an AI analysis result.
func (s *DB) SaveAnalysis(cycleID, summary string, confidence float64, anomalies interface{}, inputTokens, outputTokens, rounds int) error {
	anomaliesJSON, _ := json.Marshal(anomalies)
	_, err := s.db.Exec(`INSERT INTO analysis_history (cycle_id, health_summary, confidence, anomalies, input_tokens, output_tokens, rounds, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		cycleID, summary, confidence, string(anomaliesJSON), inputTokens, outputTokens, rounds, time.Now())
	return err
}
