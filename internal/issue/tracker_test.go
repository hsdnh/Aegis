package issue

import (
	"testing"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

func TestFingerprintDeterministic(t *testing.T) {
	labels := map[string]string{"url": "/v1/order", "method": "POST"}
	fp1 := Fingerprint("rule", "http.latency", types.SeverityWarning, labels)
	fp2 := Fingerprint("rule", "http.latency", types.SeverityWarning, labels)
	if fp1 != fp2 {
		t.Errorf("same inputs should produce same fingerprint: %s != %s", fp1, fp2)
	}
}

func TestFingerprintDifferentLabels(t *testing.T) {
	fp1 := Fingerprint("rule", "http.latency", types.SeverityWarning, map[string]string{"url": "/v1/order"})
	fp2 := Fingerprint("rule", "http.latency", types.SeverityWarning, map[string]string{"url": "/v1/search"})
	if fp1 == fp2 {
		t.Error("different labels should produce different fingerprints")
	}
}

func TestFingerprintNilLabels(t *testing.T) {
	fp1 := Fingerprint("rule", "redis.alive", types.SeverityFatal, nil)
	fp2 := Fingerprint("rule", "redis.alive", types.SeverityFatal, nil)
	if fp1 != fp2 {
		t.Errorf("nil labels should still be deterministic: %s != %s", fp1, fp2)
	}
}

func TestIssueLifecycle_CriticalImmediateOpen(t *testing.T) {
	tracker := NewTracker()
	health := types.SnapshotHealth{TotalCollectors: 1, SuccessCollectors: 1, Completeness: 1.0, Trustworthy: true}

	rr := []types.RuleResult{{
		RuleName: "test_rule", Triggered: true, Severity: types.SeverityCritical,
		MetricName: "redis.queue", MetricValue: 5000, Threshold: 1000,
	}}

	// CRITICAL: should be OPEN immediately on first detection (no DETECTING phase)
	newIss, _, _ := tracker.ProcessCycleResults("c1", rr, health)
	if len(newIss) != 1 {
		t.Fatalf("CRITICAL should create OPEN issue on first detection, got %d", len(newIss))
	}
	if newIss[0].Status != types.IssueOpen {
		t.Errorf("expected OPEN, got %s", newIss[0].Status)
	}
}

func TestIssueLifecycle_WarningDetectingToOpen(t *testing.T) {
	tracker := NewTracker()
	health := types.SnapshotHealth{TotalCollectors: 1, SuccessCollectors: 1, Completeness: 1.0, Trustworthy: true}

	rr := []types.RuleResult{{
		RuleName: "test_warn", Triggered: true, Severity: types.SeverityWarning,
		MetricName: "log.errors", MetricValue: 100, Threshold: 50,
	}}

	// WARNING: Cycle 1 should be DETECTING (not yet OPEN)
	newIss, _, _ := tracker.ProcessCycleResults("c1", rr, health)
	if len(newIss) != 0 {
		t.Error("WARNING should not create OPEN issue on first detection")
	}
	issues := tracker.OpenIssues()
	if len(issues) != 1 || issues[0].Status != types.IssueDetecting {
		t.Errorf("expected 1 DETECTING issue, got %d", len(issues))
	}

	// Cycle 2: should transition to OPEN
	newIss, _, _ = tracker.ProcessCycleResults("c2", rr, health)
	if len(newIss) != 1 {
		t.Error("should create OPEN issue after 2 consecutive detections")
	}
	if newIss[0].Status != types.IssueOpen {
		t.Errorf("expected OPEN, got %s", newIss[0].Status)
	}
}

func TestIssueLifecycle_OpenToResolved(t *testing.T) {
	tracker := NewTracker()
	health := types.SnapshotHealth{TotalCollectors: 1, SuccessCollectors: 1, Completeness: 1.0, Trustworthy: true}

	rr := []types.RuleResult{{
		RuleName: "test", Triggered: true, Severity: types.SeverityWarning,
		MetricName: "test.metric", MetricValue: 100, Threshold: 50,
	}}
	noRules := []types.RuleResult{} // empty = no anomalies this cycle

	// Open the issue
	tracker.ProcessCycleResults("c1", rr, health)
	tracker.ProcessCycleResults("c2", rr, health)

	// 3 cycles of no anomaly → should resolve
	tracker.ProcessCycleResults("c3", noRules, health)
	tracker.ProcessCycleResults("c4", noRules, health)
	_, _, resolved := tracker.ProcessCycleResults("c5", noRules, health)

	if len(resolved) != 1 {
		t.Errorf("expected 1 resolved issue, got %d", len(resolved))
	}
	if resolved[0].Status != types.IssueResolved {
		t.Errorf("expected RESOLVED, got %s", resolved[0].Status)
	}
}

func TestIssueLifecycle_DataGapReopen(t *testing.T) {
	tracker := NewTracker()
	goodHealth := types.SnapshotHealth{TotalCollectors: 2, SuccessCollectors: 2, Completeness: 1.0, Trustworthy: true}
	badHealth := types.SnapshotHealth{TotalCollectors: 2, SuccessCollectors: 0, Completeness: 0.0, Trustworthy: false}

	rr := []types.RuleResult{{
		RuleName: "test", Triggered: true, Severity: types.SeverityCritical,
		MetricName: "mysql.alive", MetricValue: 0, Threshold: 0,
	}}
	noRules := []types.RuleResult{}

	// Open issue
	tracker.ProcessCycleResults("c1", rr, goodHealth)
	tracker.ProcessCycleResults("c2", rr, goodHealth)

	// Data gap — should NOT resolve, should go to DATA_GAP
	_, updated, _ := tracker.ProcessCycleResults("c3", noRules, badHealth)
	found := false
	for _, u := range updated {
		if u.Status == types.IssueDataGap {
			found = true
		}
	}
	if !found {
		t.Error("expected DATA_GAP status when health is untrustworthy")
	}

	// Anomaly returns — should go back to OPEN from DATA_GAP
	_, updated2, _ := tracker.ProcessCycleResults("c4", rr, goodHealth)
	found = false
	for _, u := range updated2 {
		if u.Status == types.IssueOpen {
			found = true
		}
	}
	if !found {
		t.Error("expected DATA_GAP → OPEN when anomaly returns")
	}
}

func TestIssueFlapping(t *testing.T) {
	tracker := NewTracker()
	health := types.SnapshotHealth{TotalCollectors: 1, SuccessCollectors: 1, Completeness: 1.0, Trustworthy: true}

	rr := []types.RuleResult{{
		RuleName: "flappy", Triggered: true, Severity: types.SeverityWarning,
		MetricName: "test.flap", MetricValue: 100, Threshold: 50,
	}}
	noRules := []types.RuleResult{{
		RuleName: "flappy", Triggered: false, Severity: types.SeverityWarning,
		MetricName: "test.flap", MetricValue: 30, Threshold: 50,
	}}

	// Open
	tracker.ProcessCycleResults("c1", rr, health)
	tracker.ProcessCycleResults("c2", rr, health)

	// Resolve + reopen 3 times → should become FLAPPING
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			tracker.ProcessCycleResults("r", noRules, health)
		}
		tracker.ProcessCycleResults("b", rr, health)
	}

	issues := tracker.OpenIssues()
	hasFlapping := false
	for _, iss := range issues {
		if iss.Status == types.IssueFlapping {
			hasFlapping = true
		}
	}
	if !hasFlapping {
		t.Error("expected FLAPPING after repeated open/resolve cycles")
	}
}
