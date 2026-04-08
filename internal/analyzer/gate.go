package analyzer

import (
	"github.com/hsdnh/Aegis/pkg/types"
)

// ShouldInvokeAI decides if this cycle's snapshot warrants an AI analysis call.
// Goal: avoid calling AI every 30 minutes when nothing changed.
// Only call AI when there's actually something worth analyzing.
func ShouldInvokeAI(snapshot types.Snapshot, openIssues []*types.Issue, previousAlertCount int) bool {
	// 1. Any CRITICAL or FATAL rule triggered → always analyze
	for _, rr := range snapshot.RuleResults {
		if rr.Triggered && (rr.Severity == types.SeverityCritical || rr.Severity == types.SeverityFatal) {
			return true
		}
	}

	// 2. Significant increase in alerts compared to last cycle
	currentAlerts := countTriggeredRules(snapshot.RuleResults)
	if currentAlerts > 0 && currentAlerts > previousAlertCount+2 {
		return true
	}

	// 3. New alerts that weren't present before (state change)
	if currentAlerts > 0 && previousAlertCount == 0 {
		return true
	}

	// 4. Open issues exist and need periodic re-analysis (every 3rd cycle)
	// This is tracked externally — caller passes cycle count
	if len(openIssues) > 0 {
		return true
	}

	// 5. High error log volume
	for _, r := range snapshot.Results {
		for _, m := range r.Metrics {
			if m.Name == "log.lines.errors" && m.Value > 100 {
				return true
			}
		}
	}

	// 6. Snapshot health is poor (collectors failing)
	if snapshot.Health.Completeness < 0.8 && snapshot.Health.FailedCollectors > 0 {
		return true
	}

	// Nothing interesting — skip AI call this cycle
	return false
}

func countTriggeredRules(results []types.RuleResult) int {
	count := 0
	for _, rr := range results {
		if rr.Triggered {
			count++
		}
	}
	return count
}
