package issue

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

const (
	// Hysteresis thresholds — prevent flapping
	openThreshold    = 2 // consecutive bad cycles before DETECTING → OPEN
	resolveThreshold = 3 // consecutive good cycles before → RESOLVED
	closeThreshold   = 6 // consecutive good cycles before RESOLVED → CLOSED
	reopenWindow     = 24 * time.Hour
	flapThreshold    = 3 // reopen count before marking FLAPPING
	flapCooldown     = 12 * time.Hour
)

// Tracker manages the lifecycle of all issues.
type Tracker struct {
	mu     sync.RWMutex
	issues map[string]*types.Issue // keyed by fingerprint
}

func NewTracker() *Tracker {
	return &Tracker{
		issues: make(map[string]*types.Issue),
	}
}

// Fingerprint generates a stable ID from symptom characteristics.
// Format: hash(service + metricName + labels + severity)
func Fingerprint(source, metricName string, severity types.Severity, labels map[string]string) string {
	raw := fmt.Sprintf("%s|%s|%s|%v", source, metricName, severity, labels)
	h := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", h[:8])
}

// ProcessCycleResults takes triggered rule results and updates issue states.
// Returns: new issues, updated issues, resolved issues.
func (t *Tracker) ProcessCycleResults(cycleID string, ruleResults []types.RuleResult, snapshotHealth types.SnapshotHealth) (
	newIssues []*types.Issue,
	updatedIssues []*types.Issue,
	resolvedIssues []*types.Issue,
) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()

	// Collect fingerprints of currently-active anomalies
	activeFingerprints := make(map[string]types.RuleResult)
	for _, rr := range ruleResults {
		if !rr.Triggered {
			continue
		}
		fp := Fingerprint("rule", rr.MetricName, rr.Severity, nil)
		activeFingerprints[fp] = rr
	}

	// Update existing issues
	for fp, issue := range t.issues {
		if issue.Status == types.IssueClosed {
			continue
		}

		if _, stillBad := activeFingerprints[fp]; stillBad {
			// Anomaly still present
			issue.ConsecutiveBad++
			issue.ConsecutiveGood = 0
			issue.LastSeenAt = now
			issue.UpdatedAt = now

			switch issue.Status {
			case types.IssueDetecting:
				if issue.ConsecutiveBad >= openThreshold {
					t.transition(issue, types.IssueOpen, "Confirmed after repeated detection", cycleID)
					newIssues = append(newIssues, issue)
				}
			case types.IssueImproving:
				t.transition(issue, types.IssueOpen, "Regression — anomaly returned", cycleID)
				updatedIssues = append(updatedIssues, issue)
			case types.IssueResolved:
				issue.ReopenCount++
				issue.FlapScore++
				if issue.FlapScore >= flapThreshold {
					t.transition(issue, types.IssueFlapping, "Oscillating too frequently", cycleID)
				} else {
					t.transition(issue, types.IssueOpen, fmt.Sprintf("Reopened (count: %d)", issue.ReopenCount), cycleID)
				}
				updatedIssues = append(updatedIssues, issue)
			}

			delete(activeFingerprints, fp) // consumed
		} else {
			// Anomaly gone (or data gap)
			if !snapshotHealth.Trustworthy && issue.Status == types.IssueOpen {
				// Don't resolve if data is incomplete — might be false recovery
				t.transition(issue, types.IssueDataGap, "Cannot verify — collector data incomplete", cycleID)
				updatedIssues = append(updatedIssues, issue)
				continue
			}

			issue.ConsecutiveGood++
			issue.ConsecutiveBad = 0
			issue.UpdatedAt = now

			switch issue.Status {
			case types.IssueDetecting:
				// Was just a blip, never confirmed → remove
				delete(t.issues, fp)
			case types.IssueOpen, types.IssueAcked, types.IssueMonitoring:
				if issue.ConsecutiveGood >= resolveThreshold {
					resolvedTime := now
					issue.ResolvedAt = &resolvedTime
					t.transition(issue, types.IssueResolved, "Anomaly absent for sustained period", cycleID)
					resolvedIssues = append(resolvedIssues, issue)
				} else if issue.ConsecutiveGood >= 1 {
					t.transition(issue, types.IssueImproving, "Metrics improving", cycleID)
					updatedIssues = append(updatedIssues, issue)
				}
			case types.IssueResolved:
				if issue.ConsecutiveGood >= closeThreshold {
					closedTime := now
					issue.ClosedAt = &closedTime
					t.transition(issue, types.IssueClosed, "Stable — auto-closed", cycleID)
				}
			case types.IssueFlapping:
				if issue.ConsecutiveGood >= closeThreshold*2 {
					t.transition(issue, types.IssueResolved, "Stabilized after flapping", cycleID)
					resolvedIssues = append(resolvedIssues, issue)
				}
			case types.IssueDataGap:
				// Data restored and anomaly gone
				t.transition(issue, types.IssueImproving, "Data restored — anomaly not present", cycleID)
				updatedIssues = append(updatedIssues, issue)
			}
		}
	}

	// Create new issues for fingerprints not yet tracked
	for fp, rr := range activeFingerprints {
		issue := &types.Issue{
			ID:              fmt.Sprintf("ISS-%s-%s", time.Now().Format("0102"), fp[:6]),
			Fingerprint:     fp,
			Status:          types.IssueDetecting,
			Severity:        rr.Severity,
			Title:           rr.RuleName,
			Summary:         rr.Message,
			Evidence: []types.Evidence{{
				Timestamp:   now,
				Type:        "metric",
				Description: rr.Message,
				Value:       fmt.Sprintf("%.2f (threshold: %.2f)", rr.MetricValue, rr.Threshold),
				Source:      rr.MetricName,
			}},
			FirstSeenAt:     now,
			LastSeenAt:      now,
			UpdatedAt:       now,
			ConsecutiveBad:  1,
			History: []types.IssueEvent{{
				Timestamp:  now,
				FromStatus: "",
				ToStatus:   types.IssueDetecting,
				Reason:     "First detection",
				CycleID:    cycleID,
			}},
		}

		// Check if recently closed issue with same fingerprint should be reopened
		if old, exists := t.issues[fp]; exists && old.Status == types.IssueClosed {
			if now.Sub(*old.ClosedAt) < reopenWindow {
				old.ReopenCount++
				old.FlapScore++
				old.ConsecutiveBad = 1
				old.ConsecutiveGood = 0
				old.ClosedAt = nil
				old.ResolvedAt = nil
				t.transition(old, types.IssueOpen, "Reopened within 24h window", cycleID)
				updatedIssues = append(updatedIssues, old)
				continue
			}
		}

		t.issues[fp] = issue
	}

	return
}

// Mute suppresses notifications for an issue until the given time.
func (t *Tracker) Mute(fingerprint string, until time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	issue, ok := t.issues[fingerprint]
	if !ok {
		return fmt.Errorf("issue %s not found", fingerprint)
	}
	issue.MutedUntil = &until
	issue.Status = types.IssueAcked
	return nil
}

// OpenIssues returns all issues that are not closed.
func (t *Tracker) OpenIssues() []*types.Issue {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var result []*types.Issue
	for _, issue := range t.issues {
		if issue.Status != types.IssueClosed {
			result = append(result, issue)
		}
	}
	return result
}

// ShouldNotify checks if an issue should trigger a push notification.
// Caller must not hold the lock — this method acquires RLock internally.
func (t *Tracker) ShouldNotify(issue *types.Issue) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := time.Now()

	// Muted
	if issue.MutedUntil != nil && now.Before(*issue.MutedUntil) {
		return false
	}

	// Only notify on actionable states
	switch issue.Status {
	case types.IssueOpen, types.IssueResolved:
		// Always notify on new open or resolved
	case types.IssueImproving:
		// Notify once
	case types.IssueFlapping:
		// Suppress during flapping — only daily summary
		return false
	default:
		return false
	}

	// Don't spam — minimum 2 hours between notifications for same issue
	if issue.LastNotifiedAt != nil && now.Sub(*issue.LastNotifiedAt) < 2*time.Hour {
		return false
	}

	return true
}

// MarkNotified records that a notification was sent.
func (t *Tracker) MarkNotified(fingerprint string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if issue, ok := t.issues[fingerprint]; ok {
		now := time.Now()
		issue.LastNotifiedAt = &now
	}
}

func (t *Tracker) transition(issue *types.Issue, to types.IssueStatus, reason, cycleID string) {
	event := types.IssueEvent{
		Timestamp:  time.Now(),
		FromStatus: issue.Status,
		ToStatus:   to,
		Reason:     reason,
		CycleID:    cycleID,
	}
	issue.History = append(issue.History, event)
	issue.Status = to
}
