package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hsdnh/ai-ops-agent/internal/tracecollector"
	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

// Analyst performs AI-powered analysis on monitoring data.
// Supports self-correction: up to 3 rounds of analysis with follow-up data.
type Analyst struct {
	client       *Client
	maxRounds    int
	projectName  string
	domainKnowledge string // user-provided business context (runbooks)
}

// AnalysisInput is everything the Analyst needs for one cycle.
type AnalysisInput struct {
	Snapshot    types.Snapshot                `json:"snapshot"`
	OpenIssues  []*types.Issue                `json:"open_issues"`
	TraceData   *tracecollector.AnalyzedWindow `json:"trace_data,omitempty"`
	Baseline    *BaselineData                  `json:"baseline,omitempty"`
	CodeContext string                         `json:"code_context,omitempty"` // from L0 scan
}

// AnalysisResult is the structured output from the AI.
type AnalysisResult struct {
	// Detected anomalies with evidence
	Anomalies []DetectedAnomaly `json:"anomalies"`
	// Overall system health assessment
	HealthSummary string `json:"health_summary"`
	// Confidence in the overall analysis (0.0-1.0)
	Confidence float64 `json:"confidence"`
	// If confidence < 0.7, what additional data is needed
	NeedsMoreData []DataRequest `json:"needs_more_data,omitempty"`
	// Raw AI response for debugging
	RawResponse string `json:"-"`
	// Token usage
	TotalInputTokens  int `json:"total_input_tokens"`
	TotalOutputTokens int `json:"total_output_tokens"`
	// How many rounds of analysis were performed
	Rounds int `json:"rounds"`
}

// DetectedAnomaly represents one AI-identified issue.
type DetectedAnomaly struct {
	Title       string   `json:"title"`
	Severity    string   `json:"severity"` // "info", "warning", "critical", "fatal"
	RootCause   string   `json:"root_cause"`
	Evidence    []string `json:"evidence"`
	Suggestions []string `json:"suggestions"`
	CodeRefs    []string `json:"code_refs,omitempty"`
	Confidence  float64  `json:"confidence"`
	Category    string   `json:"category"` // "performance", "connectivity", "data_integrity", "security", "resource"
}

// DataRequest is what the AI asks for when confidence is low.
type DataRequest struct {
	Type        string `json:"type"`        // "log_tail", "sql_query", "redis_scan", "trace_window"
	Description string `json:"description"`
	Target      string `json:"target"`
}

// BaselineData holds historical normal values for comparison.
type BaselineData struct {
	MetricBaselines map[string]MetricBaseline `json:"metric_baselines"`
	NormalErrorRate float64                   `json:"normal_error_rate"`
	CollectedAt     time.Time                 `json:"collected_at"`
}

type MetricBaseline struct {
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"std_dev"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

func NewAnalyst(client *Client, projectName string) *Analyst {
	return &Analyst{
		client:      client,
		maxRounds:   3,
		projectName: projectName,
	}
}

// SetDomainKnowledge adds business-specific context to all analyses.
func (a *Analyst) SetDomainKnowledge(knowledge string) {
	a.domainKnowledge = knowledge
}

// Analyze runs AI analysis on the given input.
// Supports self-correction: if confidence < 0.7, it requests more data
// and re-analyzes (up to maxRounds times).
func (a *Analyst) Analyze(ctx context.Context, input AnalysisInput) (*AnalysisResult, error) {
	systemPrompt := a.buildSystemPrompt()
	userMsg := a.buildUserMessage(input)

	messages := []Message{
		{Role: "user", Content: userMsg},
	}

	var finalResult AnalysisResult

	for round := 1; round <= a.maxRounds; round++ {
		resp, err := a.client.ChatWithSystem(ctx, systemPrompt, messages)
		if err != nil {
			return nil, fmt.Errorf("round %d AI call failed: %w", round, err)
		}

		finalResult.TotalInputTokens += resp.InputTokens
		finalResult.TotalOutputTokens += resp.OutputTokens
		finalResult.Rounds = round
		finalResult.RawResponse = resp.Content

		// Parse structured response
		parsed, err := a.parseResponse(resp.Content)
		if err != nil {
			// If parsing fails, return raw text as summary
			finalResult.HealthSummary = resp.Content
			finalResult.Confidence = 0.5
			return &finalResult, nil
		}

		finalResult.Anomalies = parsed.Anomalies
		finalResult.HealthSummary = parsed.HealthSummary
		finalResult.Confidence = parsed.Confidence
		finalResult.NeedsMoreData = parsed.NeedsMoreData

		// Self-correction: if confident enough or max rounds reached, stop
		if parsed.Confidence >= 0.7 || round >= a.maxRounds || len(parsed.NeedsMoreData) == 0 {
			break
		}

		// AI wants more data — append its response and a follow-up
		messages = append(messages,
			Message{Role: "assistant", Content: resp.Content},
			Message{Role: "user", Content: a.buildFollowUp(parsed.NeedsMoreData)},
		)
	}

	return &finalResult, nil
}

func (a *Analyst) buildSystemPrompt() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf(`你是 AI Ops Agent 的分析引擎，专门监控"%s"项目的生产运行状态。

你的职责：
1. 分析系统指标和日志，识别异常模式
2. 关联多个数据源做根因分析（不是只看单个指标）
3. 给出具体的修复建议，包括代码位置
4. 评估置信度 — 不确定时明确说需要更多数据

分析原则：
- 采集失败（collector error）不等于"指标正常"，要标注数据缺失
- 关注指标之间的关联（如：下单为0 + MySQL报错 = Worker没连DB）
- 关注趋势变化而不只是当前值（从300涨到5万比一直是5万更严重）
- 区分"基础设施故障"和"业务逻辑bug"
- 如果有追踪数据，分析函数级热点和调用链

`, a.projectName))

	if a.domainKnowledge != "" {
		sb.WriteString("领域知识（业务上下文）：\n")
		sb.WriteString(a.domainKnowledge)
		sb.WriteString("\n\n")
	}

	sb.WriteString("请用以下 JSON 格式回复（用 json 代码块包裹）：\n")
	sb.WriteString("{\n")
	sb.WriteString("  \"health_summary\": \"一句话总结系统状态\",\n")
	sb.WriteString("  \"confidence\": 0.0-1.0,\n")
	sb.WriteString("  \"anomalies\": [\n")
	sb.WriteString("    {\n")
	sb.WriteString("      \"title\": \"简短标题\",\n")
	sb.WriteString("      \"severity\": \"info|warning|critical|fatal\",\n")
	sb.WriteString("      \"root_cause\": \"根因分析\",\n")
	sb.WriteString("      \"evidence\": [\"证据1\", \"证据2\"],\n")
	sb.WriteString("      \"suggestions\": [\"建议1\", \"建议2\"],\n")
	sb.WriteString("      \"code_refs\": [\"file.go:123\"],\n")
	sb.WriteString("      \"confidence\": 0.0-1.0,\n")
	sb.WriteString("      \"category\": \"performance|connectivity|data_integrity|security|resource\"\n")
	sb.WriteString("    }\n")
	sb.WriteString("  ],\n")
	sb.WriteString("  \"needs_more_data\": [\n")
	sb.WriteString("    {\"type\": \"log_tail|sql_query|redis_scan|trace_window\", \"description\": \"需要什么\", \"target\": \"具体目标\"}\n")
	sb.WriteString("  ]\n")
	sb.WriteString("}\n")

	return sb.String()
}

func (a *Analyst) buildUserMessage(input AnalysisInput) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## 监控周期 %s（%s）\n\n",
		input.Snapshot.CycleID,
		input.Snapshot.Timestamp.Format("2006-01-02 15:04:05")))

	// Snapshot health
	sb.WriteString(fmt.Sprintf("### 数据完整度\n采集器: %d/%d 成功 (%.0f%%)\n",
		input.Snapshot.Health.SuccessCollectors,
		input.Snapshot.Health.TotalCollectors,
		input.Snapshot.Health.Completeness*100))
	if !input.Snapshot.Health.Trustworthy {
		sb.WriteString("⚠️ 数据不可信 — 部分采集器失败\n")
	}
	sb.WriteString("\n")

	// Metrics
	sb.WriteString("### 指标\n")
	for _, r := range input.Snapshot.Results {
		if len(r.Errors) > 0 {
			sb.WriteString(fmt.Sprintf("**[%s] 采集失败**: %s\n", r.CollectorName, strings.Join(r.Errors, "; ")))
			continue
		}
		for _, m := range r.Metrics {
			sb.WriteString(fmt.Sprintf("- %s = %.2f", m.Name, m.Value))
			// Add baseline comparison if available
			if input.Baseline != nil {
				if bl, ok := input.Baseline.MetricBaselines[m.Name]; ok {
					deviation := (m.Value - bl.Mean) / max64(bl.StdDev, 0.01)
					if deviation > 3 || deviation < -3 {
						sb.WriteString(fmt.Sprintf(" ⚠️ (基线: %.2f±%.2f, 偏离 %.1fσ)", bl.Mean, bl.StdDev, deviation))
					}
				}
			}
			sb.WriteString("\n")
		}
	}

	// Triggered rules
	triggered := 0
	for _, rr := range input.Snapshot.RuleResults {
		if rr.Triggered {
			triggered++
		}
	}
	if triggered > 0 {
		sb.WriteString(fmt.Sprintf("\n### 触发的规则 (%d)\n", triggered))
		for _, rr := range input.Snapshot.RuleResults {
			if rr.Triggered {
				sb.WriteString(fmt.Sprintf("- [%s] %s: %s (值=%.2f, 阈值=%.2f)\n",
					rr.Severity, rr.RuleName, rr.Message, rr.MetricValue, rr.Threshold))
			}
		}
	}

	// Error logs
	var errorLogs []string
	for _, r := range input.Snapshot.Results {
		for _, l := range r.Logs {
			if l.Level == "ERROR" || l.Level == "WARNING" {
				errorLogs = append(errorLogs, fmt.Sprintf("[%s] %s: %s", l.Level, l.Source, l.Message))
			}
		}
	}
	if len(errorLogs) > 0 {
		sb.WriteString(fmt.Sprintf("\n### 错误日志 (最近 %d 条)\n", min(len(errorLogs), 50)))
		for i, log := range errorLogs {
			if i >= 50 {
				sb.WriteString(fmt.Sprintf("... 还有 %d 条\n", len(errorLogs)-50))
				break
			}
			sb.WriteString(fmt.Sprintf("- %s\n", log))
		}
	}

	// Open issues
	if len(input.OpenIssues) > 0 {
		sb.WriteString(fmt.Sprintf("\n### 当前未关闭 Issue (%d)\n", len(input.OpenIssues)))
		for _, iss := range input.OpenIssues {
			sb.WriteString(fmt.Sprintf("- [%s] %s: %s (持续 %s, 状态: %s)\n",
				iss.Severity, iss.ID, iss.Title,
				time.Since(iss.FirstSeenAt).Truncate(time.Minute),
				iss.Status))
		}
	}

	// Trace data (if available)
	if input.TraceData != nil {
		sb.WriteString(fmt.Sprintf("\n### 函数追踪数据 (窗口: %s, %d spans)\n",
			input.TraceData.Snapshot.Trigger,
			input.TraceData.Snapshot.SpanCount))

		sb.WriteString(fmt.Sprintf("Goroutines: %d, Heap: %dMB, GC Pause: %v\n",
			input.TraceData.Snapshot.Goroutines,
			input.TraceData.Snapshot.HeapAllocMB,
			time.Duration(input.TraceData.Snapshot.GCPauseNs)))

		if len(input.TraceData.HotSpots) > 0 {
			sb.WriteString("\n热点函数:\n")
			for i, hs := range input.TraceData.HotSpots {
				if i >= 10 {
					break
				}
				sb.WriteString(fmt.Sprintf("- %s: 总耗时 %v, 调用 %d 次, 平均 %v, 占比 %.1f%%\n",
					hs.FuncName, hs.TotalTime, hs.CallCount, hs.AvgTime, hs.PctOfTotal))
			}
		}

		if len(input.TraceData.Anomalies) > 0 {
			sb.WriteString("\n追踪异常:\n")
			for _, ta := range input.TraceData.Anomalies {
				sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", ta.Severity, ta.Type, ta.Description))
			}
		}
	}

	// Code context (from L0 scan)
	if input.CodeContext != "" {
		sb.WriteString("\n### 代码上下文 (L0 扫描结果)\n")
		sb.WriteString(input.CodeContext)
		sb.WriteString("\n")
	}

	return sb.String()
}

func (a *Analyst) buildFollowUp(requests []DataRequest) string {
	var sb strings.Builder
	sb.WriteString("你请求了更多数据，以下是补充信息：\n\n")
	for _, req := range requests {
		sb.WriteString(fmt.Sprintf("### %s: %s\n", req.Type, req.Description))
		sb.WriteString("（注：当前暂无法自动采集此数据，请基于已有信息给出最佳判断，")
		sb.WriteString("并注明你的不确定之处）\n\n")
	}
	return sb.String()
}

func (a *Analyst) parseResponse(content string) (*AnalysisResult, error) {
	// Extract JSON from markdown code block
	jsonStr := content
	if idx := strings.Index(content, "```json"); idx != -1 {
		jsonStr = content[idx+7:]
		if end := strings.Index(jsonStr, "```"); end != -1 {
			jsonStr = jsonStr[:end]
		}
	} else if idx := strings.Index(content, "{"); idx != -1 {
		// Try to find raw JSON
		jsonStr = content[idx:]
		depth := 0
		for i, ch := range jsonStr {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					jsonStr = jsonStr[:i+1]
					break
				}
			}
		}
	}

	var result AnalysisResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonStr)), &result); err != nil {
		return nil, fmt.Errorf("parse AI response JSON: %w", err)
	}
	return &result, nil
}

// ResultToIssueUpdates converts AI analysis results into Issue updates.
func ResultToIssueUpdates(result *AnalysisResult) []types.Issue {
	var issues []types.Issue
	for _, anomaly := range result.Anomalies {
		sev := types.SeverityWarning
		switch anomaly.Severity {
		case "info":
			sev = types.SeverityInfo
		case "critical":
			sev = types.SeverityCritical
		case "fatal":
			sev = types.SeverityFatal
		}

		var evidence []types.Evidence
		for _, e := range anomaly.Evidence {
			evidence = append(evidence, types.Evidence{
				Timestamp:   time.Now(),
				Type:        "ai_analysis",
				Description: e,
				Source:       "analyst",
			})
		}

		issues = append(issues, types.Issue{
			Severity:    sev,
			Title:       anomaly.Title,
			Summary:     anomaly.RootCause,
			Evidence:    evidence,
			RootCause:   anomaly.RootCause,
			Suggestions: anomaly.Suggestions,
			CodeRefs:    anomaly.CodeRefs,
			Confidence:  anomaly.Confidence,
		})
	}
	return issues
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
