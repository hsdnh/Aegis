package dashboard

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hsdnh/Aegis/internal/ai"
	"github.com/hsdnh/Aegis/pkg/types"
)

// ChatMessage represents one message in the conversation.
type ChatMessage struct {
	Role      string    `json:"role"` // "user" or "assistant"
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// ChatService handles AI-powered Q&A about the monitored system.
type ChatService struct {
	client   *ai.Client
	store    *Store
	mu       sync.Mutex
	history  []ChatMessage
	eventLog *EventLog
}

func NewChatService(client *ai.Client, store *Store, eventLog *EventLog) *ChatService {
	return &ChatService{
		client:   client,
		store:    store,
		eventLog: eventLog,
	}
}

// Ask sends a user question and returns the AI response.
func (cs *ChatService) Ask(ctx context.Context, question string) (string, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	systemCtx := cs.buildSystemContext()

	cs.history = append(cs.history, ChatMessage{
		Role: "user", Content: question, Timestamp: time.Now(),
	})

	var messages []ai.Message
	start := 0
	if len(cs.history) > 20 {
		start = len(cs.history) - 20
	}
	for _, msg := range cs.history[start:] {
		messages = append(messages, ai.Message{Role: msg.Role, Content: msg.Content})
	}

	resp, err := cs.client.ChatWithSystem(ctx, systemCtx, messages)
	if err != nil {
		return "", fmt.Errorf("AI chat failed: %w", err)
	}

	// Store response
	cs.history = append(cs.history, ChatMessage{
		Role: "assistant", Content: resp.Content, Timestamp: time.Now(),
	})

	// Log to event feed
	if cs.eventLog != nil {
		cs.eventLog.Add(Event{
			Type: EventChat, Icon: "💬",
			Message:  truncChat(question, 60),
			Details:  truncChat(resp.Content, 200),
			Severity: "info",
		})
	}

	return resp.Content, nil
}

// History returns recent chat messages.
func (cs *ChatService) History() []ChatMessage {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.history
}

func (cs *ChatService) buildSystemContext() string {
	var sb strings.Builder

	sb.WriteString("你是 AI Ops Agent 的交互式助手。用户会问关于被监控系统的问题。\n")
	sb.WriteString("基于以下实时数据回答，要具体、简洁、给出可操作的建议。\n\n")

	// Current metrics
	metrics := cs.store.GetMetrics()
	if len(metrics) > 0 {
		sb.WriteString("## 当前指标\n")
		for _, m := range metrics {
			sb.WriteString(fmt.Sprintf("- %s = %.2f\n", m.Name, m.Value))
		}
		sb.WriteString("\n")
	}

	// Open issues
	issues := cs.store.GetIssues()
	if len(issues) > 0 {
		sb.WriteString("## 当前 Issue\n")
		for _, iss := range issues {
			sb.WriteString(fmt.Sprintf("- [%s] %s: %s (状态: %s)\n",
				iss.Severity, iss.Title, iss.Summary, iss.Status))
		}
		sb.WriteString("\n")
	}

	// Latest AI analysis
	analysis := cs.store.GetLatestAnalysis()
	if analysis != nil && analysis.HealthSummary != "" {
		sb.WriteString("## 最近 AI 分析\n")
		sb.WriteString(analysis.HealthSummary + "\n\n")
	}

	return sb.String()
}

func truncChat(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// --- Metric Annotations ---

// MetricAnnotation is an AI-generated comment on a metric value.
type MetricAnnotation struct {
	MetricName string `json:"metric_name"`
	Value      float64 `json:"value"`
	Status     string `json:"status"` // "ok", "warning", "critical"
	Comment    string `json:"comment"`
}

// AnnotateMetrics generates short AI comments for key metrics.
// This runs locally (no AI call) using rules + baseline comparison.
func AnnotateMetrics(metrics []types.Metric, baseline *ai.BaselineData) []MetricAnnotation {
	var annotations []MetricAnnotation

	for _, m := range metrics {
		ann := MetricAnnotation{
			MetricName: m.Name,
			Value:      m.Value,
			Status:     "ok",
			Comment:    "正常",
		}

		// Connection alive checks
		if strings.HasSuffix(m.Name, ".alive") || strings.HasSuffix(m.Name, ".connection.alive") {
			if m.Value == 0 {
				ann.Status = "critical"
				ann.Comment = "连接断开!"
			} else {
				ann.Comment = "连接正常"
			}
			annotations = append(annotations, ann)
			continue
		}

		// Baseline comparison
		if baseline != nil {
			if bl, ok := baseline.MetricBaselines[m.Name]; ok {
				stdDev := bl.StdDev
				if stdDev < 0.01 {
					stdDev = 0.01
				}
				deviation := (m.Value - bl.Mean) / stdDev

				if deviation > 5 {
					ann.Status = "critical"
					ann.Comment = fmt.Sprintf("偏离基线 %.0f 倍 (基线: %.1f)", deviation, bl.Mean)
				} else if deviation > 3 {
					ann.Status = "warning"
					ann.Comment = fmt.Sprintf("高于基线 %.1fσ (基线: %.1f±%.1f)", deviation, bl.Mean, bl.StdDev)
				} else if deviation < -3 {
					ann.Status = "warning"
					ann.Comment = fmt.Sprintf("低于基线 %.1fσ (基线: %.1f±%.1f)", -deviation, bl.Mean, bl.StdDev)
				} else {
					ann.Comment = fmt.Sprintf("正常范围 (基线: %.1f±%.1f)", bl.Mean, bl.StdDev)
				}
				annotations = append(annotations, ann)
				continue
			}
		}

		// Heuristic annotations for common metric patterns
		if strings.Contains(m.Name, "queue") || strings.Contains(m.Name, "pending") {
			if m.Value > 10000 {
				ann.Status = "critical"
				ann.Comment = "严重堆积"
			} else if m.Value > 1000 {
				ann.Status = "warning"
				ann.Comment = "轻微堆积"
			} else {
				ann.Comment = "正常"
			}
		} else if strings.Contains(m.Name, "error") {
			if m.Value > 100 {
				ann.Status = "critical"
				ann.Comment = "错误量过高"
			} else if m.Value > 50 {
				ann.Status = "warning"
				ann.Comment = "错误偏多"
			}
		} else if strings.Contains(m.Name, "latency") || strings.Contains(m.Name, "_ms") {
			if m.Value > 5000 {
				ann.Status = "critical"
				ann.Comment = "响应极慢"
			} else if m.Value > 1000 {
				ann.Status = "warning"
				ann.Comment = "响应偏慢"
			}
		} else if strings.Contains(m.Name, "memory") || strings.Contains(m.Name, "bytes") {
			// Memory in bytes — check if above 80% of max if we know max
			ann.Comment = fmt.Sprintf("%.0f MB", m.Value/1024/1024)
		}

		annotations = append(annotations, ann)
	}

	return annotations
}
