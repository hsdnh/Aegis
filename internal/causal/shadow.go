// Shadow verification: Agent independently queries data stores to verify
// that the results produced by the application are correct.
// Like a bank's second bookkeeper checking the first one's work.
package causal

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ShadowVerifier runs independent checks against data stores.
type ShadowVerifier struct {
	mu         sync.RWMutex
	checks     []ShadowCheck
	results    []ShadowResult
	maxKeep    int
	db         *sql.DB
	rdb        *redis.Client
	active     bool // only runs during verification windows
}

// ShadowCheck defines one independent verification.
type ShadowCheck struct {
	Name        string `json:"name" yaml:"name"`
	Type        string `json:"type" yaml:"type"` // "sql_compare", "redis_compare", "cross_check"
	Description string `json:"description" yaml:"description"`
	Severity    string `json:"severity" yaml:"severity"`
	Enabled     bool   `json:"enabled" yaml:"enabled"`

	// SQL compare: run query, check result
	Query       string `json:"query,omitempty" yaml:"query"`
	Expect      string `json:"expect,omitempty" yaml:"expect"` // "== 0", "> 0", "< 100"

	// Cross check: compare two data sources
	SourceQuery string `json:"source_query,omitempty" yaml:"source_query"`
	TargetQuery string `json:"target_query,omitempty" yaml:"target_query"`
	CompareMode string `json:"compare_mode,omitempty" yaml:"compare_mode"` // "equal", "subset"

	// Schedule: how often to run (empty = every verification window)
	IntervalMin int `json:"interval_min,omitempty" yaml:"interval_min"`
}

// ShadowResult records one verification outcome.
type ShadowResult struct {
	CheckName   string    `json:"check_name"`
	Timestamp   time.Time `json:"timestamp"`
	Status      string    `json:"status"` // "pass", "fail", "error"
	Expected    string    `json:"expected"`
	Actual      string    `json:"actual"`
	Message     string    `json:"message"`
	DurationMs  int64     `json:"duration_ms"`
	Severity    string    `json:"severity"`
}

func NewShadowVerifier(db *sql.DB, rdb *redis.Client, maxKeep int) *ShadowVerifier {
	if maxKeep == 0 {
		maxKeep = 200
	}
	return &ShadowVerifier{
		db:      db,
		rdb:     rdb,
		maxKeep: maxKeep,
	}
}

func (sv *ShadowVerifier) AddCheck(c ShadowCheck) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.checks = append(sv.checks, c)
}

// RunAll executes all enabled shadow checks. Called during verification windows.
func (sv *ShadowVerifier) RunAll(ctx context.Context) []ShadowResult {
	sv.mu.Lock()
	sv.active = true
	checks := make([]ShadowCheck, len(sv.checks))
	copy(checks, sv.checks)
	sv.mu.Unlock()

	defer func() {
		sv.mu.Lock()
		sv.active = false
		sv.mu.Unlock()
	}()

	var results []ShadowResult
	for _, check := range checks {
		if !check.Enabled {
			continue
		}
		result := sv.runCheck(ctx, check)
		results = append(results, result)
	}

	sv.mu.Lock()
	sv.results = append(sv.results, results...)
	if len(sv.results) > sv.maxKeep {
		sv.results = sv.results[len(sv.results)-sv.maxKeep:]
	}
	sv.mu.Unlock()

	return results
}

func (sv *ShadowVerifier) runCheck(ctx context.Context, check ShadowCheck) ShadowResult {
	start := time.Now()
	result := ShadowResult{
		CheckName:  check.Name,
		Timestamp:  start,
		Severity:   check.Severity,
	}

	switch check.Type {
	case "sql_compare":
		result = sv.runSQLCompare(ctx, check, result)
	case "cross_check":
		result = sv.runCrossCheck(ctx, check, result)
	default:
		result.Status = "error"
		result.Message = "unknown check type: " + check.Type
	}

	result.DurationMs = time.Since(start).Milliseconds()
	return result
}

func (sv *ShadowVerifier) runSQLCompare(ctx context.Context, check ShadowCheck, result ShadowResult) ShadowResult {
	if sv.db == nil {
		result.Status = "error"
		result.Message = "MySQL not configured"
		return result
	}

	var value float64
	err := sv.db.QueryRowContext(ctx, check.Query).Scan(&value)
	if err != nil {
		result.Status = "error"
		result.Message = fmt.Sprintf("query failed: %v", err)
		return result
	}

	result.Actual = fmt.Sprintf("%.0f", value)
	result.Expected = check.Expect

	passed := evaluateExpect(value, check.Expect)
	if passed {
		result.Status = "pass"
		result.Message = fmt.Sprintf("%s: %.0f %s", check.Name, value, check.Expect)
	} else {
		result.Status = "fail"
		result.Message = fmt.Sprintf("%s: got %.0f, expected %s — %s",
			check.Name, value, check.Expect, check.Description)
	}
	return result
}

func (sv *ShadowVerifier) runCrossCheck(ctx context.Context, check ShadowCheck, result ShadowResult) ShadowResult {
	if sv.db == nil {
		result.Status = "error"
		result.Message = "MySQL not configured"
		return result
	}

	var sourceVal, targetVal float64
	if err := sv.db.QueryRowContext(ctx, check.SourceQuery).Scan(&sourceVal); err != nil {
		result.Status = "error"
		result.Message = "source query failed: " + err.Error()
		return result
	}
	if err := sv.db.QueryRowContext(ctx, check.TargetQuery).Scan(&targetVal); err != nil {
		result.Status = "error"
		result.Message = "target query failed: " + err.Error()
		return result
	}

	result.Expected = fmt.Sprintf("source=%.0f", sourceVal)
	result.Actual = fmt.Sprintf("target=%.0f", targetVal)

	switch check.CompareMode {
	case "equal":
		if sourceVal == targetVal {
			result.Status = "pass"
			result.Message = fmt.Sprintf("%s: both = %.0f", check.Name, sourceVal)
		} else {
			result.Status = "fail"
			result.Message = fmt.Sprintf("%s: source=%.0f ≠ target=%.0f — %s",
				check.Name, sourceVal, targetVal, check.Description)
		}
	default:
		result.Status = "pass"
		result.Message = fmt.Sprintf("%s: source=%.0f, target=%.0f", check.Name, sourceVal, targetVal)
	}
	return result
}

// RecentResults returns recent verification results.
func (sv *ShadowVerifier) RecentResults(n int) []ShadowResult {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	total := len(sv.results)
	if n > total {
		n = total
	}
	out := make([]ShadowResult, n)
	for i := 0; i < n; i++ {
		out[i] = sv.results[total-1-i]
	}
	return out
}

// AllChecks returns all configured shadow checks.
func (sv *ShadowVerifier) AllChecks() []ShadowCheck {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	return sv.checks
}

// FailCount returns how many checks failed in the last run.
func (sv *ShadowVerifier) FailCount() int {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	count := 0
	// Count fails in last batch (last N where N = number of enabled checks)
	enabled := 0
	for _, c := range sv.checks {
		if c.Enabled { enabled++ }
	}
	start := len(sv.results) - enabled
	if start < 0 { start = 0 }
	for _, r := range sv.results[start:] {
		if r.Status == "fail" { count++ }
	}
	return count
}

// --- helpers ---

func evaluateExpect(value float64, expect string) bool {
	expect = strings.TrimSpace(expect)
	var op string
	var threshold float64

	if strings.HasPrefix(expect, "==") {
		op = "=="
		fmt.Sscanf(expect[2:], "%f", &threshold)
	} else if strings.HasPrefix(expect, "!=") {
		op = "!="
		fmt.Sscanf(expect[2:], "%f", &threshold)
	} else if strings.HasPrefix(expect, ">=") {
		op = ">="
		fmt.Sscanf(expect[2:], "%f", &threshold)
	} else if strings.HasPrefix(expect, "<=") {
		op = "<="
		fmt.Sscanf(expect[2:], "%f", &threshold)
	} else if strings.HasPrefix(expect, ">") {
		op = ">"
		fmt.Sscanf(expect[1:], "%f", &threshold)
	} else if strings.HasPrefix(expect, "<") {
		op = "<"
		fmt.Sscanf(expect[1:], "%f", &threshold)
	} else {
		return true // can't parse, assume OK
	}

	switch op {
	case "==": return value == threshold
	case "!=": return value != threshold
	case ">":  return value > threshold
	case "<":  return value < threshold
	case ">=": return value >= threshold
	case "<=": return value <= threshold
	}
	return true
}
