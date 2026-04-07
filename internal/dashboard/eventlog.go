package dashboard

import (
	"fmt"
	"sync"
	"time"
)

// EventType categorizes activity feed events.
type EventType string

const (
	EventCycleStart  EventType = "cycle_start"
	EventCollected   EventType = "collected"
	EventRuleTriggered EventType = "rule_triggered"
	EventAIStart     EventType = "ai_start"
	EventAIRound     EventType = "ai_round"
	EventAIResult    EventType = "ai_result"
	EventIssueNew    EventType = "issue_new"
	EventIssueResolved EventType = "issue_resolved"
	EventIssueFlapping EventType = "issue_flapping"
	EventAlertSent   EventType = "alert_sent"
	EventTraceWindow EventType = "trace_window"
	EventCycleEnd    EventType = "cycle_end"
	EventSystemOK    EventType = "system_ok"
	EventError       EventType = "error"
	EventChat        EventType = "chat"
)

// Event is one entry in the activity feed.
type Event struct {
	ID        int       `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Type      EventType `json:"type"`
	Icon      string    `json:"icon"`
	Message   string    `json:"message"`
	Details   string    `json:"details,omitempty"`
	Severity  string    `json:"severity,omitempty"` // "info", "warning", "error", "success"
	CycleID   string    `json:"cycle_id,omitempty"`
	Cost      string    `json:"cost,omitempty"` // "$0.02"
}

// EventLog is a thread-safe ring buffer of events.
type EventLog struct {
	mu     sync.RWMutex
	events []Event
	maxLen int
	nextID int
}

func NewEventLog(maxLen int) *EventLog {
	if maxLen == 0 {
		maxLen = 500
	}
	return &EventLog{
		events: make([]Event, 0, maxLen),
		maxLen: maxLen,
	}
}

// Add appends an event to the log.
func (el *EventLog) Add(evt Event) {
	el.mu.Lock()
	defer el.mu.Unlock()
	el.nextID++
	evt.ID = el.nextID
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}
	el.events = append(el.events, evt)
	if len(el.events) > el.maxLen {
		el.events = el.events[len(el.events)-el.maxLen:]
	}
}

// Recent returns the last N events (newest first).
func (el *EventLog) Recent(n int) []Event {
	el.mu.RLock()
	defer el.mu.RUnlock()
	total := len(el.events)
	if n > total {
		n = total
	}
	// Return reversed (newest first)
	result := make([]Event, n)
	for i := 0; i < n; i++ {
		result[i] = el.events[total-1-i]
	}
	return result
}

// Since returns all events after the given ID.
func (el *EventLog) Since(afterID int) []Event {
	el.mu.RLock()
	defer el.mu.RUnlock()
	var result []Event
	for _, e := range el.events {
		if e.ID > afterID {
			result = append(result, e)
		}
	}
	return result
}

// --- Convenience methods ---

func (el *EventLog) CycleStart(cycleID string) {
	el.Add(Event{Type: EventCycleStart, Icon: "🔍", Message: "开始监控周期", CycleID: cycleID, Severity: "info"})
}

func (el *EventLog) Collected(cycleID string, metrics, collectors, failed int) {
	msg := ""
	sev := "info"
	if failed > 0 {
		msg = fmt.Sprintf("采集完成: %d 指标, %d/%d 采集器成功 (⚠️ %d 失败)",
			metrics, collectors-failed, collectors, failed)
		sev = "warning"
	} else {
		msg = fmt.Sprintf("采集完成: %d 指标, %d/%d 采集器正常", metrics, collectors, collectors)
	}
	el.Add(Event{Type: EventCollected, Icon: "📊", Message: msg, CycleID: cycleID, Severity: sev})
}

func (el *EventLog) RuleTriggered(cycleID, ruleName string, value, threshold float64) {
	msg := fmt.Sprintf("规则触发: %s (%.1f > %.1f)", ruleName, value, threshold)
	el.Add(Event{Type: EventRuleTriggered, Icon: "⚠️", Message: msg, CycleID: cycleID, Severity: "warning"})
}

func (el *EventLog) AIStart(cycleID string) {
	el.Add(Event{Type: EventAIStart, Icon: "🧠", Message: "AI 分析中...", CycleID: cycleID, Severity: "info"})
}

func (el *EventLog) AIRound(cycleID string, round int, confidence float64) {
	msg := fmt.Sprintf("AI Round %d, 置信度 %.0f%%", round, confidence*100)
	sev := "info"
	if confidence < 0.7 {
		msg += ", 请求更多数据..."
		sev = "warning"
	}
	el.Add(Event{Type: EventAIRound, Icon: "🧠", Message: msg, CycleID: cycleID, Severity: sev})
}

func (el *EventLog) AIResult(cycleID string, anomalyCount int, confidence float64, inputTokens, outputTokens int) {
	cost := float64(inputTokens)*0.000003 + float64(outputTokens)*0.000015 // sonnet pricing
	msg := ""
	sev := "info"
	if anomalyCount > 0 {
		msg = fmt.Sprintf("AI 发现 %d 个异常 (置信度 %.0f%%)", anomalyCount, confidence*100)
		sev = "warning"
	} else {
		msg = fmt.Sprintf("AI 分析完成: 未发现异常 (置信度 %.0f%%)", confidence*100)
		sev = "success"
	}
	el.Add(Event{Type: EventAIResult, Icon: "🧠", Message: msg, CycleID: cycleID, Severity: sev, Cost: fmt.Sprintf("$%.4f", cost)})
}

func (el *EventLog) AIAnomaly(cycleID, title, rootCause string, confidence float64, suggestions []string) {
	details := "根因: " + rootCause
	if len(suggestions) > 0 {
		details += "\n建议: " + suggestions[0]
	}
	sev := "error"
	if confidence < 0.7 {
		sev = "warning"
	}
	el.Add(Event{Type: EventAIResult, Icon: "🔴", Message: title, Details: details, CycleID: cycleID, Severity: sev})
}

func (el *EventLog) IssueNew(cycleID, issueID, title string) {
	el.Add(Event{Type: EventIssueNew, Icon: "🆕", Message: fmt.Sprintf("新 Issue: %s - %s", issueID, title), CycleID: cycleID, Severity: "error"})
}

func (el *EventLog) IssueResolved(cycleID, issueID, title string) {
	el.Add(Event{Type: EventIssueResolved, Icon: "✅", Message: fmt.Sprintf("Issue 已修复: %s - %s", issueID, title), CycleID: cycleID, Severity: "success"})
}

func (el *EventLog) AlertSent(cycleID, channel, title string) {
	el.Add(Event{Type: EventAlertSent, Icon: "📤", Message: fmt.Sprintf("已推送 %s: %s", channel, title), CycleID: cycleID, Severity: "info"})
}

func (el *EventLog) TraceWindow(cycleID string, spans, goroutines int) {
	el.Add(Event{Type: EventTraceWindow, Icon: "🔬", Message: fmt.Sprintf("追踪窗口: %d spans, %d goroutines", spans, goroutines), CycleID: cycleID, Severity: "info"})
}

func (el *EventLog) CycleEnd(cycleID string, durationSec float64) {
	el.Add(Event{Type: EventCycleEnd, Icon: "✅", Message: fmt.Sprintf("周期完成 (%.1fs)", durationSec), CycleID: cycleID, Severity: "success"})
}

func (el *EventLog) SystemOK(cycleID string) {
	el.Add(Event{Type: EventSystemOK, Icon: "✅", Message: "系统正常, 无异常", CycleID: cycleID, Severity: "success"})
}

func (el *EventLog) Error(cycleID, msg string) {
	el.Add(Event{Type: EventError, Icon: "❌", Message: msg, CycleID: cycleID, Severity: "error"})
}
