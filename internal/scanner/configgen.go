package scanner

import (
	"context"
	"fmt"
	"strings"

	"github.com/hsdnh/ai-ops-agent/internal/ai"
)

// GenerateConfig uses AI to analyze scan+probe results and produce a monitoring config.
// Returns: draft config YAML, analysis summary, and confidence score.
func GenerateConfig(ctx context.Context, client *ai.Client, scan *ScanResult, probe *ProbeResult, projectName string) (configYAML string, summary string, err error) {
	systemPrompt := buildConfigGenSystemPrompt()
	userMsg := buildConfigGenUserMessage(scan, probe, projectName)

	resp, err := client.ChatWithSystem(ctx, systemPrompt, []ai.Message{
		{Role: "user", Content: userMsg},
	})
	if err != nil {
		return "", "", fmt.Errorf("AI config generation failed: %w", err)
	}

	// Extract YAML from response
	configYAML = extractYAML(resp.Content)
	summary = extractSummary(resp.Content)

	return configYAML, summary, nil
}

func buildConfigGenSystemPrompt() string {
	return `你是 AI Ops Agent 的初始化引擎。你的任务是分析一个 Go 项目的代码扫描结果和运行时探测数据，然后生成一份完整的监控配置文件 (config.yaml)。

配置文件格式：
` + "```yaml" + `
project: <项目名>
schedule: "*/30 * * * *"

collectors:
  redis:
    - addr: "<地址>"
      password: "${REDIS_PASSWORD}"
      checks:
        - key_pattern: "<pattern>"
          threshold: <数值>
          alert: "<描述>"
  mysql:
    - dsn: "${MYSQL_DSN}"
      checks:
        - query: "<SQL>"
          name: "<名称>"
          threshold: <数值>
          alert: "<描述>"
  http:
    - url: "<URL>"
      checks:
        - json_path: "<path>"
          name: "<名称>"
          threshold: <数值>
          alert: "<描述>"
  log:
    - source: journalctl
      unit: <unit>
      error_patterns: [<patterns>]

rules:
  - name: "<规则名>"
    metric_name: "<指标名>"
    operator: ">"
    threshold: <数值>
    severity: "warning|critical|fatal"
    message: "<描述>"

ai:
  enabled: true
  provider: claude
  api_key: "${CLAUDE_API_KEY}"
  model: claude-sonnet-4-20250514

alerts:
  console: {}
` + "```" + `

你的分析原则：
1. 基于代码中发现的 Redis key 模式，生成对应的队列监控 check
2. 基于代码中发现的 SQL 表操作，生成对应的业务指标 check（如 pending 订单数）
3. 基于运行时探测的实际数据，设置合理的阈值（不是拍脑袋）
4. 基于发现的 API 路由，生成 HTTP 健康检查
5. 基于发现的日志模式，生成错误匹配 pattern
6. 对每个生成的规则标注置信度（你有多确定这个阈值合理）

回复格式：
1. 先用 ` + "```yaml" + ` 包裹完整的 config.yaml
2. 然后用 ## 分析摘要 标注你的分析过程和置信度
3. 标注哪些配置需要人工确认（标记为 TODO）`
}

func buildConfigGenUserMessage(scan *ScanResult, probe *ProbeResult, projectName string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# 项目: %s\n\n", projectName))
	sb.WriteString("## 代码扫描结果\n\n")
	sb.WriteString(scan.FormatForAI())
	sb.WriteString("\n")

	if probe != nil {
		sb.WriteString("## 运行时探测结果\n\n")
		sb.WriteString(probe.FormatForAI())
		sb.WriteString("\n")
	}

	sb.WriteString(`## 请求

基于以上扫描和探测结果：
1. 生成完整的 config.yaml 监控配置
2. 为每个发现的组件设置合理的监控规则和阈值
3. 标注哪些配置你有信心（基于实际数据），哪些需要人工确认
4. 特别关注：
   - 队列类 key 的堆积监控
   - 数据库关键表的业务指标
   - API 可用性和延迟
   - 进程心跳/存活检测
   - 已发现的错误日志模式`)

	return sb.String()
}

func extractYAML(content string) string {
	if idx := strings.Index(content, "```yaml"); idx != -1 {
		yamlStr := content[idx+7:]
		if end := strings.Index(yamlStr, "```"); end != -1 {
			return strings.TrimSpace(yamlStr[:end])
		}
		return strings.TrimSpace(yamlStr)
	}
	// Fallback: maybe it's raw YAML
	if strings.HasPrefix(strings.TrimSpace(content), "project:") {
		if idx := strings.Index(content, "##"); idx != -1 {
			return strings.TrimSpace(content[:idx])
		}
		return strings.TrimSpace(content)
	}
	return ""
}

func extractSummary(content string) string {
	if idx := strings.Index(content, "## 分析摘要"); idx != -1 {
		return strings.TrimSpace(content[idx:])
	}
	if idx := strings.Index(content, "## Analysis"); idx != -1 {
		return strings.TrimSpace(content[idx:])
	}
	// Return everything after the YAML block
	if idx := strings.LastIndex(content, "```"); idx != -1 {
		return strings.TrimSpace(content[idx+3:])
	}
	return ""
}
