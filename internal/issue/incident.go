package issue

import (
	"time"

	"github.com/hsdnh/Aegis/pkg/types"
)

// IncidentAggregator merges related issues into parent incidents.
// "MySQL down + orders stuck + API 500 = one root-cause incident, not three."
type IncidentAggregator struct {
	tracker *Tracker
}

func NewIncidentAggregator(tracker *Tracker) *IncidentAggregator {
	return &IncidentAggregator{tracker: tracker}
}

// Aggregate scans open issues and groups related ones under a parent incident.
// Rules:
//   - If a FATAL issue exists, all CRITICAL/WARNING issues that started within
//     5 minutes become children of the FATAL
//   - Issues sharing the same RootCause are grouped
//   - Causal chain links (CodeRefs overlap) cause grouping
func (ia *IncidentAggregator) Aggregate() []Incident {
	issues := ia.tracker.OpenIssues()
	if len(issues) < 2 {
		return nil
	}

	// Find root causes (FATAL severity = likely root)
	var roots []*types.Issue
	var children []*types.Issue
	for _, iss := range issues {
		if iss.Severity >= types.SeverityCritical && iss.ParentID == "" {
			roots = append(roots, iss)
		} else {
			children = append(children, iss)
		}
	}

	var incidents []Incident
	used := make(map[string]bool)

	for _, root := range roots {
		inc := Incident{
			ID:        "INC-" + root.ID,
			RootIssue: root,
			CreatedAt: root.FirstSeenAt,
		}

		for _, child := range children {
			if used[child.ID] {
				continue
			}
			if shouldGroup(root, child) {
				inc.ChildIssues = append(inc.ChildIssues, child)
				child.ParentID = root.ID
				used[child.ID] = true
			}
		}

		if len(inc.ChildIssues) > 0 {
			inc.Summary = buildIncidentSummary(inc)
			incidents = append(incidents, inc)
		}
	}

	return incidents
}

// Incident groups related issues under one root cause.
type Incident struct {
	ID          string         `json:"id"`
	RootIssue   *types.Issue   `json:"root_issue"`
	ChildIssues []*types.Issue `json:"child_issues"`
	Summary     string         `json:"summary"`
	CreatedAt   time.Time      `json:"created_at"`
}

// shouldGroup decides if a child issue is related to a root issue.
func shouldGroup(root, child *types.Issue) bool {
	// Time proximity: child appeared within 10 minutes of root
	timeDiff := child.FirstSeenAt.Sub(root.FirstSeenAt)
	if timeDiff < 0 {
		timeDiff = -timeDiff
	}
	if timeDiff > 10*time.Minute {
		return false
	}

	// Same root cause text
	if root.RootCause != "" && child.RootCause != "" && root.RootCause == child.RootCause {
		return true
	}

	// Code ref overlap
	if hasOverlap(root.CodeRefs, child.CodeRefs) {
		return true
	}

	// Lower severity child appeared shortly after higher severity root
	if child.Severity < root.Severity && timeDiff < 5*time.Minute {
		return true
	}

	return false
}

func hasOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]bool)
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if set[s] {
			return true
		}
	}
	return false
}

func buildIncidentSummary(inc Incident) string {
	summary := inc.RootIssue.Title
	if len(inc.ChildIssues) > 0 {
		summary += " (+ " + string(rune('0'+len(inc.ChildIssues))) + " related)"
	}
	return summary
}
