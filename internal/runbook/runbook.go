// Package runbook binds investigation actions to issue patterns.
// When an issue matches, its runbook actions appear as one-click buttons in the dashboard.
package runbook

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Runbook defines a set of investigation actions for a specific issue pattern.
type Runbook struct {
	ID          string   `json:"id" yaml:"id"`
	Name        string   `json:"name" yaml:"name"`
	MatchRules  []string `json:"match_rules" yaml:"match_rules"`   // rule names this runbook applies to
	MatchTerms  []string `json:"match_terms" yaml:"match_terms"`   // keywords in issue title/summary
	Actions     []Action `json:"actions" yaml:"actions"`
}

// Action is one investigation step that can be executed from the dashboard.
type Action struct {
	Name        string `json:"name" yaml:"name"`               // "Check worker logs"
	Icon        string `json:"icon" yaml:"icon"`               // emoji
	Type        string `json:"type" yaml:"type"`               // "shell", "sql", "redis", "http"
	Command     string `json:"command" yaml:"command"`          // the actual command/query
	Description string `json:"description" yaml:"description"` // what this does
}

// ActionResult records execution of a runbook action.
type ActionResult struct {
	RunbookID  string    `json:"runbook_id"`
	ActionName string    `json:"action_name"`
	Output     string    `json:"output"`
	Error      string    `json:"error,omitempty"`
	Duration   int64     `json:"duration_ms"`
	Timestamp  time.Time `json:"timestamp"`
}

// Registry holds all configured runbooks.
type Registry struct {
	runbooks []Runbook
}

func NewRegistry() *Registry {
	return &Registry{}
}

func (r *Registry) Add(rb Runbook) {
	r.runbooks = append(r.runbooks, rb)
}

// MatchIssue returns all runbooks that apply to the given issue.
func (r *Registry) MatchIssue(ruleName, title, summary string) []Runbook {
	var matched []Runbook
	lower := strings.ToLower(title + " " + summary)

	for _, rb := range r.runbooks {
		// Match by rule name
		for _, mr := range rb.MatchRules {
			if mr == ruleName {
				matched = append(matched, rb)
				goto next
			}
		}
		// Match by keywords
		for _, term := range rb.MatchTerms {
			if strings.Contains(lower, strings.ToLower(term)) {
				matched = append(matched, rb)
				goto next
			}
		}
	next:
	}
	return matched
}

// Execute runs a single action and returns the result.
func (r *Registry) Execute(ctx context.Context, rbID, actionName string) *ActionResult {
	result := &ActionResult{
		RunbookID:  rbID,
		ActionName: actionName,
		Timestamp:  time.Now(),
	}

	// Find the action
	var action *Action
	for _, rb := range r.runbooks {
		if rb.ID != rbID {
			continue
		}
		for i, a := range rb.Actions {
			if a.Name == actionName {
				action = &rb.Actions[i]
				break
			}
		}
	}
	if action == nil {
		result.Error = "action not found"
		return result
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	start := time.Now()

	switch action.Type {
	case "shell":
		result.Output = executeShell(ctx, action.Command)
	case "http":
		result.Output = fmt.Sprintf("(HTTP action: %s — implement via HTTP client)", action.Command)
	default:
		result.Error = "unsupported action type: " + action.Type
	}

	result.Duration = time.Since(start).Milliseconds()
	return result
}

// AllRunbooks returns all configured runbooks.
func (r *Registry) AllRunbooks() []Runbook {
	return r.runbooks
}

func executeShell(ctx context.Context, cmd string) string {
	var shell, flag string
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/c"
	} else {
		shell, flag = "bash", "-c"
	}

	out, err := exec.CommandContext(ctx, shell, flag, cmd).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("error: %v\n%s", err, string(out))
	}
	output := string(out)
	if len(output) > 4000 {
		output = output[:4000] + "\n... (truncated)"
	}
	return output
}

// DefaultRunbooks returns common runbooks for typical Go+Redis+MySQL projects.
func DefaultRunbooks() []Runbook {
	return []Runbook{
		{
			ID: "rb-worker-logs", Name: "Worker Log Check",
			MatchTerms: []string{"worker", "连接", "connection", "timeout"},
			Actions: []Action{
				{Name: "Recent error logs", Icon: "📋", Type: "shell",
					Command: "journalctl --no-pager --since '30 min ago' -u '*worker*' | grep -i 'error\\|panic\\|fatal' | tail -50",
					Description: "Show recent worker error logs"},
				{Name: "Worker process status", Icon: "🔍", Type: "shell",
					Command: "ps aux | grep -i worker | grep -v grep",
					Description: "Check if worker processes are running"},
			},
		},
		{
			ID: "rb-mysql-check", Name: "MySQL Connectivity",
			MatchRules: []string{"MySQL 连接断开", "mysql_down"},
			Actions: []Action{
				{Name: "Test MySQL connection", Icon: "🔌", Type: "shell",
					Command: "mysqladmin ping 2>&1 || echo 'MySQL unreachable'",
					Description: "Quick MySQL ping test"},
				{Name: "MySQL process list", Icon: "📊", Type: "shell",
					Command: "mysql -e 'SHOW PROCESSLIST' 2>&1 | head -20",
					Description: "Show active MySQL connections"},
			},
		},
		{
			ID: "rb-redis-check", Name: "Redis Health",
			MatchRules: []string{"Redis 连接断开", "Redis 队列堆积", "redis_down"},
			Actions: []Action{
				{Name: "Redis ping", Icon: "🏓", Type: "shell",
					Command: "redis-cli ping 2>&1",
					Description: "Quick Redis connectivity test"},
				{Name: "Redis queue lengths", Icon: "📏", Type: "shell",
					Command: "redis-cli --scan --pattern '*queue*' | head -20 | while read k; do echo \"$k: $(redis-cli llen $k)\"; done",
					Description: "Show all queue lengths"},
				{Name: "Redis memory", Icon: "💾", Type: "shell",
					Command: "redis-cli info memory | head -10",
					Description: "Redis memory usage"},
			},
		},
		{
			ID: "rb-trace", Name: "Performance Trace",
			MatchTerms: []string{"堆积", "慢", "slow", "latency", "timeout"},
			Actions: []Action{
				{Name: "Trigger 30s trace", Icon: "🔬", Type: "shell",
					Command: "echo '{\"action\":\"start_window\",\"duration\":30000000000,\"trigger\":\"runbook\"}' | nc -u -w1 127.0.0.1 19877",
					Description: "Capture function-level trace for 30 seconds"},
				{Name: "Top CPU processes", Icon: "📈", Type: "shell",
					Command: "ps aux --sort=-%cpu | head -10",
					Description: "Show top CPU consumers"},
			},
		},
	}
}
