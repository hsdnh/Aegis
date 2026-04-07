// AI Terminal: an interactive AI assistant accessible from the dashboard.
// Like having an AI SRE sitting on the server — you ask questions in the panel,
// it can read logs, query databases, check processes, and investigate issues.
package dashboard

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hsdnh/ai-ops-agent/internal/ai"
)

// AITerminal is a server-side AI assistant that can execute investigation commands.
type AITerminal struct {
	client   *ai.Client
	store    *Store
	eventLog *EventLog
	mu       sync.Mutex
	sessions map[string]*TerminalSession
}

// TerminalSession holds one conversation with context.
type TerminalSession struct {
	ID       string            `json:"id"`
	Messages []TerminalMessage `json:"messages"`
	Created  time.Time         `json:"created"`
	LastUsed time.Time         `json:"last_used"`
}

// TerminalMessage is one message in the terminal conversation.
type TerminalMessage struct {
	Role      string    `json:"role"` // "user", "assistant", "system", "tool_result"
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
	ToolUsed  string    `json:"tool_used,omitempty"` // which tool was invoked
}

// ToolCall represents an AI-requested investigation action.
type ToolCall struct {
	Tool   string `json:"tool"`   // "shell", "mysql", "redis", "read_file", "grep_log"
	Args   string `json:"args"`   // command or query
	Result string `json:"result"` // output
	Error  string `json:"error,omitempty"`
}

func NewAITerminal(client *ai.Client, store *Store, eventLog *EventLog) *AITerminal {
	return &AITerminal{
		client:   client,
		store:    store,
		eventLog: eventLog,
		sessions: make(map[string]*TerminalSession),
	}
}

// Ask processes a user question, optionally executing server-side commands.
func (t *AITerminal) Ask(ctx context.Context, sessionID, question string) (*TerminalMessage, error) {
	t.mu.Lock()
	session, ok := t.sessions[sessionID]
	if !ok {
		session = &TerminalSession{
			ID:      sessionID,
			Created: time.Now(),
		}
		t.sessions[sessionID] = session
	}
	session.LastUsed = time.Now()
	session.Messages = append(session.Messages, TerminalMessage{
		Role: "user", Content: question, Timestamp: time.Now(),
	})
	t.mu.Unlock()

	// Build system prompt with available tools and current system state
	t.client.SetSystemPrompt(t.buildSystemPrompt())

	// Build messages
	var msgs []ai.Message
	t.mu.Lock()
	for _, m := range session.Messages {
		if m.Role == "user" || m.Role == "assistant" {
			msgs = append(msgs, ai.Message{Role: m.Role, Content: m.Content})
		}
	}
	t.mu.Unlock()

	// First AI call — it may request tool execution
	resp, err := t.client.Chat(ctx, msgs)
	if err != nil {
		return nil, fmt.Errorf("AI call failed: %w", err)
	}

	response := resp.Content

	// Check if AI wants to run a command (look for ```tool blocks)
	if toolCalls := extractToolCalls(response); len(toolCalls) > 0 {
		// Execute tools and send results back
		var toolResults strings.Builder
		toolResults.WriteString("Tool execution results:\n\n")

		for _, tc := range toolCalls {
			result := t.executeTool(ctx, tc)
			toolResults.WriteString(fmt.Sprintf("### %s: %s\n```\n%s\n```\n\n", tc.Tool, tc.Args, result))
		}

		// Send tool results back to AI for final analysis
		msgs = append(msgs,
			ai.Message{Role: "assistant", Content: response},
			ai.Message{Role: "user", Content: toolResults.String() + "\nBased on these results, provide your analysis."},
		)

		resp2, err := t.client.Chat(ctx, msgs)
		if err == nil {
			response = resp2.Content
		}
	}

	msg := TerminalMessage{
		Role: "assistant", Content: response, Timestamp: time.Now(),
	}

	t.mu.Lock()
	session.Messages = append(session.Messages, msg)
	t.mu.Unlock()

	return &msg, nil
}

// GetSession returns a session's messages.
func (t *AITerminal) GetSession(sessionID string) *TerminalSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessions[sessionID]
}

func (t *AITerminal) buildSystemPrompt() string {
	var sb strings.Builder

	sb.WriteString(`You are an AI SRE assistant running directly on the production server.
You can investigate issues by executing commands. Available tools:

To use a tool, write a code block with the tool name:

` + "```tool:shell" + `
command here
` + "```" + `

Available tools:
- tool:shell — Run a shell command (read-only: ps, netstat, df, top, cat, grep, head, tail, journalctl, systemctl status)
- tool:mysql — Run a MySQL query (SELECT only)
- tool:redis — Run a Redis command (read-only: GET, KEYS, LLEN, HGETALL, INFO, DBSIZE)
- tool:grep_log — Search recent logs for a pattern

Security rules:
- NEVER run destructive commands (rm, DROP, DELETE, FLUSHALL, kill, shutdown)
- ONLY read-only operations
- NEVER expose passwords or secrets in your responses
- If a command fails, explain why and suggest an alternative

`)

	// Add current system context
	metrics := t.store.GetMetrics()
	if len(metrics) > 0 {
		sb.WriteString("## Current Metrics\n")
		for _, m := range metrics {
			sb.WriteString(fmt.Sprintf("- %s = %.2f\n", m.Name, m.Value))
		}
		sb.WriteString("\n")
	}

	issues := t.store.GetIssues()
	if len(issues) > 0 {
		sb.WriteString("## Open Issues\n")
		for _, iss := range issues {
			sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", iss.Severity, iss.Title, iss.Summary))
		}
	}

	return sb.String()
}

// executeTool runs an AI-requested investigation command with safety checks.
func (t *AITerminal) executeTool(ctx context.Context, tc ToolCall) string {
	// Safety timeout
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	switch tc.Tool {
	case "shell":
		return t.execShell(ctx, tc.Args)
	case "mysql":
		return t.execMySQL(ctx, tc.Args)
	case "redis":
		return t.execRedis(ctx, tc.Args)
	case "grep_log":
		return t.execGrepLog(ctx, tc.Args)
	default:
		return "unknown tool: " + tc.Tool
	}
}

func (t *AITerminal) execShell(ctx context.Context, cmdStr string) string {
	// Whitelist safe commands
	cmd := strings.Fields(cmdStr)
	if len(cmd) == 0 {
		return "empty command"
	}

	safe := map[string]bool{
		"ps": true, "top": true, "df": true, "free": true, "uptime": true,
		"netstat": true, "ss": true, "cat": true, "head": true, "tail": true,
		"grep": true, "wc": true, "ls": true, "find": true, "du": true,
		"journalctl": true, "systemctl": true, "date": true, "hostname": true,
		"whoami": true, "uname": true, "lsof": true, "dig": true, "curl": true,
	}

	if !safe[cmd[0]] {
		return fmt.Sprintf("blocked: '%s' is not in the safe command whitelist", cmd[0])
	}

	// Block dangerous flags
	for _, arg := range cmd {
		if strings.Contains(arg, "rm ") || strings.Contains(arg, "--delete") ||
			strings.Contains(arg, "> /") || strings.Contains(arg, "| sh") {
			return "blocked: potentially destructive argument detected"
		}
	}

	var shell, flag string
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/c"
	} else {
		shell, flag = "bash", "-c"
	}

	out, err := exec.CommandContext(ctx, shell, flag, cmdStr).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("error: %v\noutput: %s", err, truncate(string(out), 2000))
	}
	return truncate(string(out), 4000)
}

func (t *AITerminal) execMySQL(ctx context.Context, query string) string {
	upper := strings.ToUpper(strings.TrimSpace(query))
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "SHOW") &&
		!strings.HasPrefix(upper, "DESCRIBE") && !strings.HasPrefix(upper, "EXPLAIN") {
		return "blocked: only SELECT/SHOW/DESCRIBE/EXPLAIN queries allowed"
	}
	return "(MySQL execution requires DB connection — configure in shadow verifier)"
}

func (t *AITerminal) execRedis(ctx context.Context, cmdStr string) string {
	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		return "empty command"
	}
	upper := strings.ToUpper(parts[0])
	safe := map[string]bool{
		"GET": true, "MGET": true, "KEYS": true, "SCAN": true, "TYPE": true,
		"LLEN": true, "LRANGE": true, "SCARD": true, "SMEMBERS": true,
		"HGET": true, "HGETALL": true, "HLEN": true, "ZCARD": true,
		"DBSIZE": true, "INFO": true, "TTL": true, "EXISTS": true, "STRLEN": true,
	}
	if !safe[upper] {
		return fmt.Sprintf("blocked: '%s' is not a safe read-only Redis command", upper)
	}
	return "(Redis execution requires client connection — configure in shadow verifier)"
}

func (t *AITerminal) execGrepLog(ctx context.Context, pattern string) string {
	if pattern == "" {
		return "empty pattern"
	}
	// Search journalctl for the pattern
	cmd := fmt.Sprintf("journalctl --no-pager --since '30 min ago' | grep -i '%s' | tail -50",
		strings.ReplaceAll(pattern, "'", ""))
	return t.execShell(ctx, cmd)
}

// extractToolCalls parses AI response for tool invocation blocks.
func extractToolCalls(response string) []ToolCall {
	var calls []ToolCall
	lines := strings.Split(response, "\n")
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "```tool:") {
			tool := strings.TrimPrefix(line, "```tool:")
			tool = strings.TrimSuffix(tool, "```")
			// Collect command until closing ```
			var cmdLines []string
			i++
			for i < len(lines) && strings.TrimSpace(lines[i]) != "```" {
				cmdLines = append(cmdLines, lines[i])
				i++
			}
			if len(cmdLines) > 0 {
				calls = append(calls, ToolCall{
					Tool: strings.TrimSpace(tool),
					Args: strings.TrimSpace(strings.Join(cmdLines, "\n")),
				})
			}
		}
		i++
	}
	return calls
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}
