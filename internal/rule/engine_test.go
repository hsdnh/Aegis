package rule

import (
	"testing"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

func TestEvaluateGreaterThan(t *testing.T) {
	rules := []Rule{
		{Name: "queue_high", MetricName: "redis.queue", Operator: ">", Threshold: 1000, SeverityStr: "critical"},
	}
	engine := NewEngine(rules)

	metrics := []types.Metric{
		{Name: "redis.queue", Value: 5000, Source: "redis"},
	}

	results := engine.Evaluate(metrics)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Triggered {
		t.Error("5000 > 1000 should trigger")
	}
	if results[0].Labels == nil {
		// Labels should be passed through from metric
	}
}

func TestEvaluateEqual(t *testing.T) {
	rules := []Rule{
		{Name: "mysql_down", MetricName: "mysql.alive", Operator: "==", Threshold: 0, SeverityStr: "fatal"},
	}
	engine := NewEngine(rules)

	metrics := []types.Metric{
		{Name: "mysql.alive", Value: 0, Source: "mysql"},
	}

	results := engine.Evaluate(metrics)
	if len(results) != 1 || !results[0].Triggered {
		t.Error("0 == 0 should trigger")
	}
}

func TestEvaluateNotTriggered(t *testing.T) {
	rules := []Rule{
		{Name: "queue_high", MetricName: "redis.queue", Operator: ">", Threshold: 1000, SeverityStr: "warning"},
	}
	engine := NewEngine(rules)

	metrics := []types.Metric{
		{Name: "redis.queue", Value: 500, Source: "redis"},
	}

	results := engine.Evaluate(metrics)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Triggered {
		t.Error("500 > 1000 should NOT trigger")
	}
}

func TestEvaluateWithLabels(t *testing.T) {
	rules := []Rule{
		{Name: "api_slow", MetricName: "http.latency", Operator: ">", Threshold: 5000, SeverityStr: "warning",
			Labels: map[string]string{"url": "/v1/order"}},
	}
	engine := NewEngine(rules)

	metrics := []types.Metric{
		{Name: "http.latency", Value: 8000, Labels: map[string]string{"url": "/v1/order"}, Source: "http"},
		{Name: "http.latency", Value: 8000, Labels: map[string]string{"url": "/v1/search"}, Source: "http"},
	}

	results := engine.Evaluate(metrics)
	triggered := 0
	for _, r := range results {
		if r.Triggered {
			triggered++
			if r.Labels["url"] != "/v1/order" {
				t.Error("triggered rule should have matching labels")
			}
		}
	}
	if triggered != 1 {
		t.Errorf("expected 1 triggered (only /v1/order matches), got %d", triggered)
	}
}

func TestSuppression(t *testing.T) {
	d := NewDependencyMap()
	d.AddDependency("mysql.alive", []string{"mysql.orders", "mysql.tasks"})

	results := []types.RuleResult{
		{RuleName: "mysql_down", Triggered: true, Severity: types.SeverityFatal, MetricName: "mysql.alive"},
		{RuleName: "orders_stuck", Triggered: true, Severity: types.SeverityWarning, MetricName: "mysql.orders"},
		{RuleName: "tasks_stuck", Triggered: true, Severity: types.SeverityCritical, MetricName: "mysql.tasks"},
	}

	filtered, suppressed := d.Suppress(results)

	if len(suppressed) != 2 {
		t.Errorf("expected 2 suppressed, got %d", len(suppressed))
	}

	triggeredCount := 0
	for _, r := range filtered {
		if r.Triggered {
			triggeredCount++
		}
	}
	if triggeredCount != 1 {
		t.Errorf("expected 1 triggered after suppression, got %d", triggeredCount)
	}
}
