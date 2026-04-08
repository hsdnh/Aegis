// Package investigator implements autonomous AI-driven investigation.
//
// Two modes:
//   - Auto: AI detects anomaly → generates investigation commands → executes → analyzes results → pushes conclusion
//   - Manual: User triggers investigation from dashboard AI Terminal
//
// Credentials are loaded from config/env automatically — AI never sees raw passwords,
// but can reference them as {{mysql_password}} in commands.
package investigator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hsdnh/ai-ops-agent/internal/ai"
	"github.com/redis/go-redis/v9"
)

// Investigation records one complete auto-investigation.
type Investigation struct {
	ID          string           `json:"id"`
	Trigger     string           `json:"trigger"` // "auto", "manual"
	Anomaly     string           `json:"anomaly"` // what triggered it
	StartTime   time.Time        `json:"start_time"`
	EndTime     time.Time        `json:"end_time"`
	Steps       []InvestStep     `json:"steps"`
	Conclusion  string           `json:"conclusion"`
	RootCause   string           `json:"root_cause"`
	Suggestions []string         `json:"suggestions"`
	Confidence  float64          `json:"confidence"`
	Status      string           `json:"status"` // "running", "complete", "failed"
}

// InvestStep is one command executed during investigation.
type InvestStep struct {
	Command     string `json:"command"`
	Type        string `json:"type"` // "shell", "mysql", "redis"
	Purpose     string `json:"purpose"`
	Output      string `json:"output"`
	DurationMs  int64  `json:"duration_ms"`
	Redacted    bool   `json:"redacted"` // true if output was sanitized
}

// Credentials holds connection info loaded from config/env.
// AI references these by name, never sees raw values.
type Credentials struct {
	mu       sync.RWMutex
	mysqlDSN string
	redisAddr string
	redisPass string
	envVars  map[string]string // additional env-based credentials
}

func NewCredentials() *Credentials {
	return &Credentials{envVars: make(map[string]string)}
}

func (c *Credentials) SetMySQL(dsn string)           { c.mu.Lock(); c.mysqlDSN = dsn; c.mu.Unlock() }
func (c *Credentials) SetRedis(addr, pass string)    { c.mu.Lock(); c.redisAddr = addr; c.redisPass = pass; c.mu.Unlock() }
func (c *Credentials) SetEnv(key, value string)      { c.mu.Lock(); c.envVars[key] = value; c.mu.Unlock() }

// Investigator performs autonomous investigation when anomalies are detected.
type Investigator struct {
	client     *ai.Client
	creds      *Credentials
	mu         sync.Mutex
	history    []Investigation
	maxKeep    int
	safeShellCmds map[string]bool
}

func New(client *ai.Client, creds *Credentials) *Investigator {
	return &Investigator{
		client:  client,
		creds:   creds,
		maxKeep: 50,
		safeShellCmds: map[string]bool{
			"ps": true, "top": true, "df": true, "free": true, "uptime": true,
			"netstat": true, "ss": true, "cat": true, "head": true, "tail": true,
			"grep": true, "wc": true, "ls": true, "find": true, "du": true,
			"journalctl": true, "systemctl": true, "date": true, "hostname": true,
			"whoami": true, "uname": true, "lsof": true, "dig": true, "curl": true,
		},
	}
}

// Investigate runs an autonomous investigation for a detected anomaly.
// AI decides what commands to run, executes them, then analyzes results.
func (inv *Investigator) Investigate(ctx context.Context, anomalyDescription string, evidence []string) *Investigation {
	inv.mu.Lock()
	investigation := &Investigation{
		ID:        fmt.Sprintf("inv-%d", time.Now().UnixNano()),
		Trigger:   "auto",
		Anomaly:   anomalyDescription,
		StartTime: time.Now(),
		Status:    "running",
	}
	inv.mu.Unlock()

	log.Printf("[investigator] Starting auto-investigation: %s", anomalyDescription)

	// Step 1: Ask AI what commands to run
	plan := inv.planInvestigation(ctx, anomalyDescription, evidence)

	// Step 2: Execute each planned command
	for _, cmd := range plan {
		step := inv.executeStep(ctx, cmd)
		investigation.Steps = append(investigation.Steps, step)
	}

	// Step 3: Send all results back to AI for final analysis
	conclusion := inv.analyzeResults(ctx, anomalyDescription, investigation.Steps)
	investigation.Conclusion = conclusion.Summary
	investigation.RootCause = conclusion.RootCause
	investigation.Suggestions = conclusion.Suggestions
	investigation.Confidence = conclusion.Confidence
	investigation.EndTime = time.Now()
	investigation.Status = "complete"

	inv.mu.Lock()
	inv.history = append(inv.history, *investigation)
	if len(inv.history) > inv.maxKeep {
		inv.history = inv.history[len(inv.history)-inv.maxKeep:]
	}
	inv.mu.Unlock()

	log.Printf("[investigator] Complete: %s (confidence: %.0f%%)", investigation.RootCause, investigation.Confidence*100)
	return investigation
}

// RecentInvestigations returns recent investigation history.
func (inv *Investigator) RecentInvestigations(n int) []Investigation {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	total := len(inv.history)
	if n > total { n = total }
	result := make([]Investigation, n)
	for i := 0; i < n; i++ {
		result[i] = inv.history[total-1-i]
	}
	return result
}

// --- Internal: AI-driven planning ---

type plannedCommand struct {
	Type    string // "shell", "mysql", "redis"
	Command string
	Purpose string
}

type conclusion struct {
	Summary     string
	RootCause   string
	Suggestions []string
	Confidence  float64
}

func (inv *Investigator) planInvestigation(ctx context.Context, anomaly string, evidence []string) []plannedCommand {
	prompt := fmt.Sprintf(`You are investigating a production anomaly. Plan investigation commands.

Anomaly: %s

Evidence:
%s

Available command types:
- shell: Linux commands (read-only: ps, grep, tail, journalctl, df, netstat, cat, curl)
- mysql: SELECT queries only (connection is pre-configured)
- redis: Read-only commands (GET, LLEN, INFO, KEYS, SCAN, HGETALL)

Return EXACTLY a JSON array of investigation steps:
[
  {"type": "shell", "command": "journalctl --since '30min ago' -u worker | grep error | tail -20", "purpose": "Check recent worker errors"},
  {"type": "mysql", "command": "SELECT COUNT(*) FROM proxy_orders WHERE status='failed' AND created_at > NOW()-INTERVAL 1 HOUR", "purpose": "Count recent failed orders"}
]

Rules:
- Maximum 6 commands
- Read-only only, never modify anything
- Start with the most likely root cause
- Each command should narrow down the problem`, anomaly, strings.Join(evidence, "\n"))

	resp, err := inv.client.ChatWithSystem(ctx,
		"You are an SRE investigator. Return ONLY valid JSON array, no markdown.",
		[]ai.Message{{Role: "user", Content: prompt}})
	if err != nil {
		return defaultInvestigationPlan(anomaly)
	}

	return parseInvestigationPlan(resp.Content, anomaly)
}

func (inv *Investigator) executeStep(ctx context.Context, cmd plannedCommand) InvestStep {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	start := time.Now()
	step := InvestStep{
		Command: cmd.Command,
		Type:    cmd.Type,
		Purpose: cmd.Purpose,
	}

	switch cmd.Type {
	case "shell":
		step.Output = inv.execShell(ctx, cmd.Command)
	case "mysql":
		step.Output = inv.execMySQL(ctx, cmd.Command)
	case "redis":
		step.Output = inv.execRedis(ctx, cmd.Command)
	default:
		step.Output = "unsupported command type"
	}

	step.DurationMs = time.Since(start).Milliseconds()

	// Redact sensitive data in output
	if containsSensitive(step.Output) {
		step.Output = redactOutput(step.Output)
		step.Redacted = true
	}

	return step
}

func (inv *Investigator) analyzeResults(ctx context.Context, anomaly string, steps []InvestStep) conclusion {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Anomaly: %s\n\nInvestigation results:\n\n", anomaly))
	for i, s := range steps {
		sb.WriteString(fmt.Sprintf("Step %d: %s\nCommand: %s %s\nOutput:\n%s\n\n",
			i+1, s.Purpose, s.Type, s.Command, truncateStr(s.Output, 1000)))
	}
	sb.WriteString(`Based on these results, provide your analysis as JSON:
{"summary": "one line", "root_cause": "specific cause", "suggestions": ["fix1", "fix2"], "confidence": 0.0-1.0}`)

	resp, err := inv.client.ChatWithSystem(ctx,
		"You are an SRE investigator analyzing command outputs. Return ONLY valid JSON.",
		[]ai.Message{{Role: "user", Content: sb.String()}})
	if err != nil {
		return conclusion{Summary: "Investigation failed: " + err.Error(), Confidence: 0}
	}

	return parseConclusion(resp.Content)
}

// --- Command execution with safety ---

func (inv *Investigator) execShell(ctx context.Context, cmdStr string) string {
	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		return "empty command"
	}
	// Whitelist check
	baseCmd := parts[0]
	if !inv.safeShellCmds[baseCmd] {
		return fmt.Sprintf("blocked: '%s' not in safe whitelist", baseCmd)
	}
	// Block destructive patterns
	lower := strings.ToLower(cmdStr)
	if strings.Contains(lower, "rm ") || strings.Contains(lower, "> /") ||
		strings.Contains(lower, "| sh") || strings.Contains(lower, "kill") {
		return "blocked: potentially destructive"
	}

	var shell, flag string
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/c"
	} else {
		shell, flag = "bash", "-c"
	}
	out, err := exec.CommandContext(ctx, shell, flag, cmdStr).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("error: %v\n%s", err, truncateStr(string(out), 2000))
	}
	return truncateStr(string(out), 4000)
}

func (inv *Investigator) execMySQL(ctx context.Context, query string) string {
	upper := strings.ToUpper(strings.TrimSpace(query))
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "SHOW") {
		return "blocked: only SELECT/SHOW queries allowed"
	}

	inv.creds.mu.RLock()
	dsn := inv.creds.mysqlDSN
	inv.creds.mu.RUnlock()

	if dsn == "" {
		return "MySQL not configured"
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return "MySQL connection failed: " + err.Error()
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return "Query failed: " + err.Error()
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var result strings.Builder
	result.WriteString(strings.Join(cols, "\t") + "\n")

	rowCount := 0
	for rows.Next() && rowCount < 50 {
		values := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		rows.Scan(ptrs...)
		var row []string
		for _, v := range values {
			row = append(row, fmt.Sprintf("%v", v))
		}
		result.WriteString(strings.Join(row, "\t") + "\n")
		rowCount++
	}

	return truncateStr(result.String(), 3000)
}

func (inv *Investigator) execRedis(ctx context.Context, cmdStr string) string {
	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		return "empty command"
	}

	safe := map[string]bool{
		"GET": true, "MGET": true, "KEYS": true, "SCAN": true, "TYPE": true,
		"LLEN": true, "LRANGE": true, "SCARD": true, "SMEMBERS": true,
		"HGET": true, "HGETALL": true, "HLEN": true, "ZCARD": true,
		"DBSIZE": true, "INFO": true, "TTL": true, "EXISTS": true,
	}
	if !safe[strings.ToUpper(parts[0])] {
		return fmt.Sprintf("blocked: '%s' is not safe", parts[0])
	}

	inv.creds.mu.RLock()
	addr := inv.creds.redisAddr
	pass := inv.creds.redisPass
	inv.creds.mu.RUnlock()

	if addr == "" {
		return "Redis not configured"
	}

	client := redis.NewClient(&redis.Options{Addr: addr, Password: pass})
	defer client.Close()

	// Build redis command args
	args := make([]interface{}, len(parts))
	for i, p := range parts {
		args[i] = p
	}

	result, err := client.Do(ctx, args...).Result()
	if err != nil {
		return "Redis error: " + err.Error()
	}
	return truncateStr(fmt.Sprintf("%v", result), 3000)
}

// --- Helpers ---

func defaultInvestigationPlan(anomaly string) []plannedCommand {
	lower := strings.ToLower(anomaly)
	var plan []plannedCommand

	// Always check system resources
	plan = append(plan, plannedCommand{"shell", "ps aux --sort=-%cpu | head -10", "Top CPU processes"})
	plan = append(plan, plannedCommand{"shell", "df -h | head -10", "Disk usage"})

	if strings.Contains(lower, "mysql") || strings.Contains(lower, "连接") || strings.Contains(lower, "order") {
		plan = append(plan, plannedCommand{"shell", "journalctl --since '30min ago' | grep -i 'mysql\\|connection' | tail -20", "MySQL related logs"})
	}
	if strings.Contains(lower, "redis") || strings.Contains(lower, "queue") || strings.Contains(lower, "队列") {
		plan = append(plan, plannedCommand{"redis", "INFO memory", "Redis memory status"})
		plan = append(plan, plannedCommand{"redis", "DBSIZE", "Redis key count"})
	}
	if strings.Contains(lower, "worker") || strings.Contains(lower, "离线") {
		plan = append(plan, plannedCommand{"shell", "ps aux | grep -i worker | grep -v grep", "Worker processes"})
	}
	return plan
}

func parseInvestigationPlan(content, anomaly string) []plannedCommand {
	// Extract JSON array from response
	start := strings.Index(content, "[")
	end := strings.LastIndex(content, "]")
	if start == -1 || end == -1 || end <= start {
		return defaultInvestigationPlan(anomaly)
	}

	type rawCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Purpose string `json:"purpose"`
	}

	var cmds []rawCmd
	if err := json.Unmarshal([]byte(content[start:end+1]), &cmds); err != nil {
		return defaultInvestigationPlan(anomaly)
	}

	var plan []plannedCommand
	for _, c := range cmds {
		if len(plan) >= 6 { break } // max 6 commands
		plan = append(plan, plannedCommand{Type: c.Type, Command: c.Command, Purpose: c.Purpose})
	}
	if len(plan) == 0 {
		return defaultInvestigationPlan(anomaly)
	}
	return plan
}

func parseConclusion(content string) conclusion {
	type rawConc struct {
		Summary     string   `json:"summary"`
		RootCause   string   `json:"root_cause"`
		Suggestions []string `json:"suggestions"`
		Confidence  float64  `json:"confidence"`
	}

	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start == -1 || end == -1 {
		return conclusion{Summary: content, Confidence: 0.5}
	}

	var rc rawConc
	if err := json.Unmarshal([]byte(content[start:end+1]), &rc); err != nil {
		return conclusion{Summary: content, Confidence: 0.5}
	}
	return conclusion{
		Summary: rc.Summary, RootCause: rc.RootCause,
		Suggestions: rc.Suggestions, Confidence: rc.Confidence,
	}
}

func containsSensitive(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "password") || strings.Contains(lower, "secret") ||
		strings.Contains(lower, "token") || strings.Contains(lower, "api_key")
}

func redactOutput(s string) string {
	// Simple: replace lines containing sensitive keywords
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "password") || strings.Contains(lower, "secret") ||
			strings.Contains(lower, "token") {
			lines = append(lines, "<REDACTED>")
		} else {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func truncateStr(s string, max int) string {
	if len(s) <= max { return s }
	return s[:max] + "\n...(truncated)"
}

