// Synthetic probes — actively test system functionality by sending real requests.
// Like a doctor telling you to run on a treadmill to see if your heart is ok.
package causal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// SyntheticProbe defines one active health check.
type SyntheticProbe struct {
	Name        string         `json:"name" yaml:"name"`
	Type        string         `json:"type" yaml:"type"` // "http", "redis_roundtrip", "mysql_roundtrip", "custom"
	Schedule    string         `json:"schedule" yaml:"schedule"` // cron expression
	Config      ProbeConfig    `json:"config" yaml:"config"`
	LastResult  *ProbeResult   `json:"last_result,omitempty"`
	Enabled     bool           `json:"enabled" yaml:"enabled"`
}

type ProbeConfig struct {
	// HTTP probe
	URL        string            `json:"url,omitempty" yaml:"url"`
	Method     string            `json:"method,omitempty" yaml:"method"`
	Headers    map[string]string `json:"headers,omitempty" yaml:"headers"`
	Body       string            `json:"body,omitempty" yaml:"body"`
	ExpectStatus int             `json:"expect_status,omitempty" yaml:"expect_status"`
	ExpectBody   string          `json:"expect_body,omitempty" yaml:"expect_body"`
	TimeoutSec   int             `json:"timeout_sec,omitempty" yaml:"timeout_sec"`

	// Redis roundtrip probe
	RedisAddr  string `json:"redis_addr,omitempty" yaml:"redis_addr"`
	RedisPass  string `json:"redis_pass,omitempty" yaml:"redis_pass"`
	RedisKey   string `json:"redis_key,omitempty" yaml:"redis_key"` // key to write→read→delete

	// Multi-step probe
	Steps []ProbeStepConfig `json:"steps,omitempty" yaml:"steps"`
}

type ProbeStepConfig struct {
	Name       string `json:"name" yaml:"name"`
	Type       string `json:"type" yaml:"type"` // "http", "redis_check", "mysql_check", "wait"
	URL        string `json:"url,omitempty" yaml:"url"`
	Query      string `json:"query,omitempty" yaml:"query"`
	Key        string `json:"key,omitempty" yaml:"key"`
	Expect     string `json:"expect,omitempty" yaml:"expect"`
	WaitMs     int    `json:"wait_ms,omitempty" yaml:"wait_ms"`
}

// ProbeResult records the outcome of one probe execution.
type ProbeResult struct {
	ProbeName string       `json:"probe_name"`
	Timestamp time.Time    `json:"timestamp"`
	Status    string       `json:"status"` // "pass", "fail", "timeout"
	Duration  int64        `json:"duration_ms"`
	Steps     []StepResult `json:"steps,omitempty"`
	Error     string       `json:"error,omitempty"`
}

type StepResult struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // "pass", "fail"
	Duration  int64  `json:"duration_ms"`
	Expected  string `json:"expected,omitempty"`
	Actual    string `json:"actual,omitempty"`
	Error     string `json:"error,omitempty"`
}

// SyntheticRunner manages and executes synthetic probes.
type SyntheticRunner struct {
	probes  []SyntheticProbe
	results []ProbeResult // ring buffer
	maxKeep int
	client  *http.Client
}

func NewSyntheticRunner() *SyntheticRunner {
	return &SyntheticRunner{
		maxKeep: 100,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (sr *SyntheticRunner) AddProbe(p SyntheticProbe) {
	sr.probes = append(sr.probes, p)
}

// RunAll executes all enabled probes and returns results.
func (sr *SyntheticRunner) RunAll(ctx context.Context) []ProbeResult {
	var results []ProbeResult
	for _, p := range sr.probes {
		if !p.Enabled {
			continue
		}
		result := sr.runProbe(ctx, p)
		results = append(results, result)
	}
	sr.results = append(sr.results, results...)
	if len(sr.results) > sr.maxKeep {
		sr.results = sr.results[len(sr.results)-sr.maxKeep:]
	}
	return results
}

// RunOne executes a single probe by name.
func (sr *SyntheticRunner) RunOne(ctx context.Context, name string) *ProbeResult {
	for _, p := range sr.probes {
		if p.Name == name {
			result := sr.runProbe(ctx, p)
			sr.results = append(sr.results, result)
			return &result
		}
	}
	return nil
}

// RecentResults returns recent probe results.
func (sr *SyntheticRunner) RecentResults(n int) []ProbeResult {
	if n > len(sr.results) {
		n = len(sr.results)
	}
	result := make([]ProbeResult, n)
	for i := 0; i < n; i++ {
		result[i] = sr.results[len(sr.results)-1-i]
	}
	return result
}

// AllProbes returns all configured probes with their last results.
func (sr *SyntheticRunner) AllProbes() []SyntheticProbe {
	return sr.probes
}

func (sr *SyntheticRunner) runProbe(ctx context.Context, p SyntheticProbe) ProbeResult {
	start := time.Now()
	result := ProbeResult{
		ProbeName: p.Name,
		Timestamp: start,
		Status:    "pass",
	}

	timeout := time.Duration(p.Config.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch p.Type {
	case "http":
		sr.runHTTPProbe(ctx, p.Config, &result)
	case "redis_roundtrip":
		sr.runRedisRoundtrip(ctx, p.Config, &result)
	case "multi_step":
		sr.runMultiStep(ctx, p.Config, &result)
	default:
		result.Status = "fail"
		result.Error = "unknown probe type: " + p.Type
	}

	result.Duration = time.Since(start).Milliseconds()
	return result
}

func (sr *SyntheticRunner) runHTTPProbe(ctx context.Context, cfg ProbeConfig, result *ProbeResult) {
	method := cfg.Method
	if method == "" {
		method = "GET"
	}

	var body io.Reader
	if cfg.Body != "" {
		body = strings.NewReader(cfg.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, cfg.URL, body)
	if err != nil {
		result.Status = "fail"
		result.Error = err.Error()
		return
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := sr.client.Do(req)
	if err != nil {
		result.Status = "fail"
		result.Error = err.Error()
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// Check status code
	if cfg.ExpectStatus > 0 && resp.StatusCode != cfg.ExpectStatus {
		result.Status = "fail"
		result.Error = fmt.Sprintf("expected status %d, got %d", cfg.ExpectStatus, resp.StatusCode)
		result.Steps = append(result.Steps, StepResult{
			Name: "status_code", Status: "fail",
			Expected: fmt.Sprintf("%d", cfg.ExpectStatus),
			Actual:   fmt.Sprintf("%d", resp.StatusCode),
		})
		return
	}
	result.Steps = append(result.Steps, StepResult{
		Name: "status_code", Status: "pass",
		Actual: fmt.Sprintf("%d", resp.StatusCode),
	})

	// Check body contains expected string
	if cfg.ExpectBody != "" && !strings.Contains(string(respBody), cfg.ExpectBody) {
		result.Status = "fail"
		result.Steps = append(result.Steps, StepResult{
			Name: "body_check", Status: "fail",
			Expected: cfg.ExpectBody,
			Actual:   truncStr(string(respBody), 200),
		})
		return
	}
	if cfg.ExpectBody != "" {
		result.Steps = append(result.Steps, StepResult{
			Name: "body_check", Status: "pass",
		})
	}
}

func (sr *SyntheticRunner) runRedisRoundtrip(ctx context.Context, cfg ProbeConfig, result *ProbeResult) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPass,
	})
	defer client.Close()

	testKey := cfg.RedisKey
	if testKey == "" {
		testKey = "__aiops_probe_test"
	}
	testVal := fmt.Sprintf("probe-%d", time.Now().UnixNano())

	// Write
	start := time.Now()
	err := client.Set(ctx, testKey, testVal, 30*time.Second).Err()
	writeDur := time.Since(start).Milliseconds()
	if err != nil {
		result.Status = "fail"
		result.Error = "write failed: " + err.Error()
		result.Steps = append(result.Steps, StepResult{Name: "write", Status: "fail", Error: err.Error(), Duration: writeDur})
		return
	}
	result.Steps = append(result.Steps, StepResult{Name: "write", Status: "pass", Duration: writeDur})

	// Read back
	start = time.Now()
	got, err := client.Get(ctx, testKey).Result()
	readDur := time.Since(start).Milliseconds()
	if err != nil || got != testVal {
		result.Status = "fail"
		result.Steps = append(result.Steps, StepResult{
			Name: "read", Status: "fail", Expected: testVal, Actual: got, Duration: readDur,
		})
		return
	}
	result.Steps = append(result.Steps, StepResult{Name: "read", Status: "pass", Duration: readDur})

	// Cleanup
	client.Del(ctx, testKey)
	result.Steps = append(result.Steps, StepResult{Name: "cleanup", Status: "pass"})
}

func (sr *SyntheticRunner) runMultiStep(ctx context.Context, cfg ProbeConfig, result *ProbeResult) {
	for _, step := range cfg.Steps {
		start := time.Now()
		sr := StepResult{Name: step.Name}

		switch step.Type {
		case "http":
			resp, err := http.Get(step.URL)
			if err != nil {
				sr.Status = "fail"
				sr.Error = err.Error()
			} else {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				sr.Status = "pass"
				sr.Actual = truncStr(string(body), 100)
				if step.Expect != "" && !strings.Contains(string(body), step.Expect) {
					sr.Status = "fail"
					sr.Expected = step.Expect
				}
			}
		case "wait":
			time.Sleep(time.Duration(step.WaitMs) * time.Millisecond)
			sr.Status = "pass"
		default:
			sr.Status = "fail"
			sr.Error = "unknown step type"
		}

		sr.Duration = time.Since(start).Milliseconds()
		result.Steps = append(result.Steps, sr)

		if sr.Status == "fail" {
			result.Status = "fail"
			result.Error = fmt.Sprintf("step '%s' failed: %s", step.Name, sr.Error)
			return // stop on first failure
		}
	}
}

// --- API response helpers ---

// ProbesSummary returns a dashboard-friendly summary.
type ProbesSummary struct {
	TotalProbes  int           `json:"total_probes"`
	EnabledCount int           `json:"enabled_count"`
	PassCount    int           `json:"pass_count"`
	FailCount    int           `json:"fail_count"`
	Probes       []ProbeStatus `json:"probes"`
}

type ProbeStatus struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Enabled    bool   `json:"enabled"`
	LastStatus string `json:"last_status"`
	LastDuration int64 `json:"last_duration_ms"`
	LastRun    *time.Time `json:"last_run,omitempty"`
}

func (sr *SyntheticRunner) Summary() ProbesSummary {
	summary := ProbesSummary{TotalProbes: len(sr.probes)}
	for _, p := range sr.probes {
		ps := ProbeStatus{Name: p.Name, Type: p.Type, Enabled: p.Enabled}
		if p.Enabled {
			summary.EnabledCount++
		}
		if p.LastResult != nil {
			ps.LastStatus = p.LastResult.Status
			ps.LastDuration = p.LastResult.Duration
			ps.LastRun = &p.LastResult.Timestamp
			if p.LastResult.Status == "pass" {
				summary.PassCount++
			} else {
				summary.FailCount++
			}
		}
		summary.Probes = append(summary.Probes, ps)
	}
	return summary
}

func truncStr(s string, max int) string {
	if len(s) <= max { return s }
	return s[:max] + "..."
}

// MarshalJSON is a helper to make ProbeResult serializable when it contains steps.
func (pr ProbeResult) ToJSON() string {
	b, _ := json.Marshal(pr)
	return string(b)
}
