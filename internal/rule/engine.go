package rule

import (
	"fmt"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

// Operator defines comparison operations for rules.
type Operator string

const (
	OpGreaterThan Operator = ">"
	OpLessThan    Operator = "<"
	OpEqual       Operator = "=="
	OpNotEqual    Operator = "!="
)

// Rule defines a threshold-based alerting rule.
type Rule struct {
	Name       string         `yaml:"name"`
	MetricName string         `yaml:"metric_name"`
	Operator   Operator       `yaml:"operator"`
	Threshold  float64        `yaml:"threshold"`
	Severity   types.Severity `yaml:"-"`
	SeverityStr string        `yaml:"severity"`
	Message    string         `yaml:"message"`
	Labels     map[string]string `yaml:"labels,omitempty"`
}

// Engine evaluates rules against collected metrics.
type Engine struct {
	rules []Rule
}

func NewEngine(rules []Rule) *Engine {
	// Parse severity strings
	for i := range rules {
		rules[i].Severity = parseSeverity(rules[i].SeverityStr)
	}
	return &Engine{rules: rules}
}

// Evaluate checks all rules against the given metrics and returns triggered results.
func (e *Engine) Evaluate(metrics []types.Metric) []types.RuleResult {
	var results []types.RuleResult

	metricMap := make(map[string][]types.Metric)
	for _, m := range metrics {
		metricMap[m.Name] = append(metricMap[m.Name], m)
	}

	for _, rule := range e.rules {
		matchedMetrics, ok := metricMap[rule.MetricName]
		if !ok {
			continue
		}

		for _, metric := range matchedMetrics {
			// Check label match
			if !matchLabels(metric.Labels, rule.Labels) {
				continue
			}

			triggered := evaluate(metric.Value, rule.Operator, rule.Threshold)
			results = append(results, types.RuleResult{
				RuleName:    rule.Name,
				Triggered:   triggered,
				Severity:    rule.Severity,
				Message:     formatMessage(rule.Message, metric),
				MetricName:  metric.Name,
				MetricValue: metric.Value,
				Threshold:   rule.Threshold,
			})
		}
	}

	return results
}

func evaluate(value float64, op Operator, threshold float64) bool {
	switch op {
	case OpGreaterThan:
		return value > threshold
	case OpLessThan:
		return value < threshold
	case OpEqual:
		return value == threshold
	case OpNotEqual:
		return value != threshold
	default:
		return false
	}
}

func matchLabels(metricLabels, ruleLabels map[string]string) bool {
	for k, v := range ruleLabels {
		if metricLabels[k] != v {
			return false
		}
	}
	return true
}

func formatMessage(template string, metric types.Metric) string {
	if template == "" {
		return fmt.Sprintf("%s = %.2f", metric.Name, metric.Value)
	}
	return fmt.Sprintf("%s (当前值: %.2f)", template, metric.Value)
}

func parseSeverity(s string) types.Severity {
	switch s {
	case "info", "INFO":
		return types.SeverityInfo
	case "warning", "WARNING":
		return types.SeverityWarning
	case "critical", "CRITICAL":
		return types.SeverityCritical
	case "fatal", "FATAL":
		return types.SeverityFatal
	default:
		return types.SeverityWarning
	}
}
