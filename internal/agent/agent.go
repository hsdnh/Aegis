package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hsdnh/ai-ops-agent/internal/ai"
	"github.com/hsdnh/ai-ops-agent/internal/alert"
	"github.com/hsdnh/ai-ops-agent/internal/analyzer"
	"github.com/hsdnh/ai-ops-agent/internal/causal"
	"github.com/hsdnh/ai-ops-agent/internal/collector"
	"github.com/hsdnh/ai-ops-agent/internal/dashboard"
	"github.com/hsdnh/ai-ops-agent/internal/health"
	"github.com/hsdnh/ai-ops-agent/internal/investigator"
	"github.com/hsdnh/ai-ops-agent/internal/issue"
	"github.com/hsdnh/ai-ops-agent/internal/rule"
	"github.com/hsdnh/ai-ops-agent/internal/sanitize"
	"github.com/hsdnh/ai-ops-agent/internal/storage"
	"github.com/hsdnh/ai-ops-agent/internal/tracecollector"
	"github.com/hsdnh/ai-ops-agent/pkg/types"
	"github.com/google/uuid"
)

// Agent is the main orchestrator that runs the monitoring loop.
type Agent struct {
	projectName    string
	collectors     *collector.Registry
	ruleEngine     *rule.Engine
	depMap         *rule.DependencyMap
	alertMgr       *alert.Manager
	issueTracker   *issue.Tracker
	analyst         *ai.Analyst                   // L2 AI analysis (nil = disabled)
	causalGraph     *causal.Graph                 // bidirectional causal tracing (nil = disabled)
	traceReceiver   *tracecollector.TraceReceiver
	traceTargetAddr string
	codeContext     string
	store           *dashboard.Store    // dashboard data (nil = no dashboard)
	eventLog        *dashboard.EventLog // activity feed (nil = no dashboard)
	db              *storage.DB                    // SQLite persistence (nil = memory only)
	baselineLearner *storage.BaselineLearner       // baseline learning (nil = disabled)
	expectModel     *causal.ExpectationModel       // trace pattern learning (nil = disabled)
	shadowVerifier  *causal.ShadowVerifier         // independent data verification (nil = disabled)
	syntheticRunner *causal.SyntheticRunner         // active probes (nil = disabled)
	autoInvestigator *investigator.Investigator     // autonomous investigation (nil = disabled)
	logger          *log.Logger
	prevAlertCount  int
}

func New(
	projectName string,
	collectors *collector.Registry,
	ruleEngine *rule.Engine,
	depMap *rule.DependencyMap,
	alertMgr *alert.Manager,
	issueTracker *issue.Tracker,
) *Agent {
	return &Agent{
		projectName:  projectName,
		collectors:   collectors,
		ruleEngine:   ruleEngine,
		depMap:       depMap,
		alertMgr:     alertMgr,
		issueTracker: issueTracker,
		logger:       log.New(log.Writer(), "[ai-ops-agent] ", log.LstdFlags),
	}
}

// RunOnce executes a single monitoring cycle:
// self-check → collect → rules → suppress → issue lifecycle → AI → notify → dashboard
func (a *Agent) RunOnce(ctx context.Context) (*types.Snapshot, error) {
	cycleStart := time.Now()
	cycleID := uuid.New().String()[:8]
	a.logger.Printf("=== Cycle %s started ===", cycleID)
	a.emit(func(el *dashboard.EventLog) { el.CycleStart(cycleID) })

	// Step 0: Agent self-health check
	agentHealth := health.CheckSelf(ctx)
	if !agentHealth.NetworkOK {
		a.logger.Printf("WARNING: Agent network check failed — suppressing CRITICAL alerts this cycle")
	}

	snapshot := &types.Snapshot{
		ProjectName: a.projectName,
		CycleID:     cycleID,
		Timestamp:   time.Now(),
	}

	// Step 1: Collect metrics and logs (parallel, with per-collector timeouts)
	a.logger.Printf("Collecting from %d collectors...", len(a.collectors.All()))
	results, snapshotHealth := a.collectors.CollectAll(ctx)
	snapshot.Results = results
	snapshot.Health = snapshotHealth

	// Emit collection result to activity feed
	var totalMetrics, totalLogs int
	for _, r := range snapshot.Results {
		totalMetrics += len(r.Metrics)
		totalLogs += len(r.Logs)
		for _, e := range r.Errors {
			a.logger.Printf("  [%s] ERROR: %s", r.CollectorName, e)
		}
	}
	a.logger.Printf("Collected: %d metrics, %d logs (health: %.0f%%)",
		totalMetrics, totalLogs, snapshotHealth.Completeness*100)
	a.emit(func(el *dashboard.EventLog) {
		el.Collected(cycleID, totalMetrics, snapshotHealth.TotalCollectors, snapshotHealth.FailedCollectors)
	})

	// Step 2: Gather all metrics for rule evaluation
	var allMetrics []types.Metric
	for _, r := range snapshot.Results {
		allMetrics = append(allMetrics, r.Metrics...)
	}

	// Step 3: Evaluate rules
	ruleResults := a.ruleEngine.Evaluate(allMetrics)

	// Step 4: Dependency-aware suppression
	if a.depMap != nil {
		filtered, suppressed := a.depMap.Suppress(ruleResults)
		ruleResults = filtered
		for _, s := range suppressed {
			a.logger.Printf("  SUPPRESSED: %s (root cause already reported)", s.RuleName)
		}
	}

	// Step 5: If agent network is down, suppress FATAL alerts
	if health.ShouldSuppressCritical(agentHealth) {
		for i := range ruleResults {
			if ruleResults[i].Triggered && ruleResults[i].Severity == types.SeverityFatal {
				ruleResults[i].Triggered = false
				a.logger.Printf("  SUPPRESSED (network partition): %s", ruleResults[i].RuleName)
			}
		}
	}

	snapshot.RuleResults = ruleResults

	triggeredCount := 0
	for _, rr := range snapshot.RuleResults {
		if rr.Triggered {
			triggeredCount++
			a.logger.Printf("  RULE TRIGGERED: %s - %s (value=%.2f, threshold=%.2f)",
				rr.RuleName, rr.Message, rr.MetricValue, rr.Threshold)
			a.emit(func(el *dashboard.EventLog) {
				el.RuleTriggered(cycleID, rr.RuleName, rr.MetricValue, rr.Threshold)
			})
		}
	}

	// Step 5.5: Causal chain tracing — link anomalies back to code
	if a.causalGraph != nil && triggeredCount > 0 {
		a.causalGraph.EnrichFromMetrics(allMetrics, snapshot.RuleResults)
		if a.traceReceiver != nil {
			a.causalGraph.EnrichFromTrace(a.traceReceiver.LatestWindow())
		}
		chains := a.causalGraph.TraceAllAnomalies(snapshot)
		for _, ch := range chains {
			a.logger.Printf("  CAUSAL CHAIN: %s → %s (confidence %.0f%%)",
				ch.Symptom, ch.RootCause, ch.Confidence*100)
			a.emit(func(el *dashboard.EventLog) {
				el.Add(dashboard.Event{
					Type: "causal", Icon: "🔗",
					Message:  fmt.Sprintf("因果链: %s → %s", ch.Symptom, ch.RootCause),
					Details:  formatChainSteps(ch.Chain),
					Severity: "info", CycleID: cycleID,
				})
			})
		}
	}

	// Step 6: Issue lifecycle management
	newIssues, updatedIssues, resolvedIssues := a.issueTracker.ProcessCycleResults(
		cycleID, snapshot.RuleResults, snapshot.Health)

	a.logger.Printf("Issues: %d new, %d updated, %d resolved, %d open total",
		len(newIssues), len(updatedIssues), len(resolvedIssues),
		len(a.issueTracker.OpenIssues()))

	for _, iss := range newIssues {
		a.emit(func(el *dashboard.EventLog) { el.IssueNew(cycleID, iss.ID, iss.Title) })
	}
	for _, iss := range resolvedIssues {
		a.emit(func(el *dashboard.EventLog) { el.IssueResolved(cycleID, iss.ID, iss.Title) })
	}

	// Step 7: Generate notifications from issue state changes
	var toNotify []types.Alert
	for _, iss := range newIssues {
		if a.issueTracker.ShouldNotify(iss) {
			toNotify = append(toNotify, issueToAlert(iss, a.projectName))
			a.issueTracker.MarkNotified(iss.Fingerprint)
		}
	}
	// Notify on updates (regression, worsening, data gap recovery)
	for _, iss := range updatedIssues {
		if a.issueTracker.ShouldNotify(iss) {
			al := issueToAlert(iss, a.projectName)
			al.Title = fmt.Sprintf("[%s] %s", iss.Status, al.Title)
			toNotify = append(toNotify, al)
			a.issueTracker.MarkNotified(iss.Fingerprint)
		}
	}
	for _, iss := range resolvedIssues {
		if a.issueTracker.ShouldNotify(iss) {
			al := issueToAlert(iss, a.projectName)
			al.Title = "[RESOLVED] " + al.Title
			al.Severity = types.SeverityInfo
			toNotify = append(toNotify, al)
			a.issueTracker.MarkNotified(iss.Fingerprint)
		}
	}
	snapshot.Alerts = toNotify

	// Step 8: Sanitize before any external use
	sanitizedSnapshot := sanitize.SanitizeSnapshot(*snapshot)

	// Step 8.5: Baseline learning — ingest metrics and compute baselines
	var aiBaseline *ai.BaselineData
	if a.baselineLearner != nil {
		a.baselineLearner.Ingest(allMetrics)
		if a.baselineLearner.IsReady() {
			aiBaseline = a.baselineLearner.GetAIBaseline()
		}
	}

	// Step 9: Shadow verification — independently verify data integrity
	if a.shadowVerifier != nil {
		svResults := a.shadowVerifier.RunAll(ctx)
		for _, svr := range svResults {
			if svr.Status == "fail" {
				a.logger.Printf("  SHADOW FAIL: %s — %s", svr.CheckName, svr.Message)
				a.emit(func(el *dashboard.EventLog) {
					el.Add(dashboard.Event{Type: "shadow", Icon: "🔍", Message: "Shadow: " + svr.Message, Severity: "error", CycleID: cycleID})
				})
			}
		}
	}

	// Step 9.5: Synthetic probes — active health checks
	if a.syntheticRunner != nil {
		probeResults := a.syntheticRunner.RunAll(ctx)
		for _, pr := range probeResults {
			if pr.Status == "fail" {
				a.logger.Printf("  PROBE FAIL: %s — %s", pr.ProbeName, pr.Error)
				a.emit(func(el *dashboard.EventLog) {
					el.Add(dashboard.Event{Type: "probe", Icon: "🧪", Message: "Probe failed: " + pr.ProbeName + " — " + pr.Error, Severity: "error", CycleID: cycleID})
				})
			}
		}
	}

	// Step 10: L2 AI Analysis (event-driven)
	if a.analyst != nil && analyzer.ShouldInvokeAI(*snapshot, a.issueTracker.OpenIssues(), a.prevAlertCount) {
		a.logger.Printf("Invoking AI analysis...")
		a.emit(func(el *dashboard.EventLog) { el.AIStart(cycleID) })

		aiInput := ai.AnalysisInput{
			Snapshot:    sanitizedSnapshot,
			OpenIssues:  a.issueTracker.OpenIssues(),
			CodeContext: a.codeContext,
			Baseline:    aiBaseline, // ← NOW WIRED: baseline data feeds AI
		}
		if a.traceReceiver != nil {
			aiInput.TraceData = a.traceReceiver.LatestWindow()
		}

		aiResult, err := a.analyst.Analyze(ctx, aiInput)
		if err != nil {
			a.logger.Printf("  AI analysis failed: %v", err)
			a.emit(func(el *dashboard.EventLog) { el.Error(cycleID, "AI analysis failed: "+err.Error()) })
		} else {
			a.logger.Printf("  AI analysis: %d anomalies, confidence %.0f%%, %d rounds",
				len(aiResult.Anomalies), aiResult.Confidence*100, aiResult.Rounds)

			a.emit(func(el *dashboard.EventLog) {
				el.AIResult(cycleID, len(aiResult.Anomalies), aiResult.Confidence,
					aiResult.TotalInputTokens, aiResult.TotalOutputTokens)
			})

			// ← NOW WIRED: AI anomalies enrich existing Issues with root cause + suggestions
			for _, anomaly := range aiResult.Anomalies {
				if anomaly.Confidence >= 0.6 {
					a.emit(func(el *dashboard.EventLog) {
						el.AIAnomaly(cycleID, anomaly.Title, anomaly.RootCause, anomaly.Confidence, anomaly.Suggestions)
					})
					// Enrich matching open issues — match by title similarity, not blind first-one
					for _, iss := range a.issueTracker.OpenIssues() {
						if iss.Status != types.IssueOpen || iss.RootCause != "" {
							continue
						}
						// Match: anomaly title contains issue title or vice versa
						if strings.Contains(strings.ToLower(anomaly.Title), strings.ToLower(iss.Title)) ||
							strings.Contains(strings.ToLower(iss.Title), strings.ToLower(anomaly.Title)) ||
							strings.Contains(anomaly.RootCause, iss.Title) {
							iss.RootCause = anomaly.RootCause
							iss.Suggestions = anomaly.Suggestions
							iss.CodeRefs = anomaly.CodeRefs
							iss.Confidence = anomaly.Confidence
							break
						}
					}
				}
			}

			snapshot.Analysis = &types.AIAnalysis{
				Summary:       aiResult.HealthSummary,
				Confidence:    aiResult.Confidence,
				AffectedAreas: collectAreas(aiResult),
			}
			if len(aiResult.Anomalies) > 0 {
				snapshot.Analysis.RootCause = aiResult.Anomalies[0].RootCause
				for _, anom := range aiResult.Anomalies {
					snapshot.Analysis.Suggestions = append(snapshot.Analysis.Suggestions, anom.Suggestions...)
				}
			}

			if a.store != nil {
				a.store.PushAnalysis(aiResult, cycleID)
			}
			// ← NOW WIRED: persist AI analysis to SQLite
			if a.db != nil {
				a.db.SaveAnalysis(cycleID, aiResult.HealthSummary, aiResult.Confidence,
					aiResult.Anomalies, aiResult.TotalInputTokens, aiResult.TotalOutputTokens, aiResult.Rounds)
			}
		}
	}

	// Step 10.3: Autonomous investigation for high-severity anomalies
	if a.autoInvestigator != nil && len(newIssues) > 0 {
		for _, iss := range newIssues {
			if iss.Severity >= types.SeverityCritical {
				a.logger.Printf("Auto-investigating: %s", iss.Title)
				a.emit(func(el *dashboard.EventLog) {
					el.Add(dashboard.Event{Type: "investigate", Icon: "🕵️",
						Message: "Auto-investigating: " + iss.Title, Severity: "info", CycleID: cycleID})
				})
				var evidence []string
				for _, ev := range iss.Evidence {
					evidence = append(evidence, ev.Description)
				}
				inv := a.autoInvestigator.Investigate(ctx, iss.Title+": "+iss.Summary, evidence)
				if inv.RootCause != "" {
					iss.RootCause = inv.RootCause
					iss.Suggestions = inv.Suggestions
					iss.Confidence = inv.Confidence
				}
				a.emit(func(el *dashboard.EventLog) {
					el.Add(dashboard.Event{Type: "investigate", Icon: "🕵️",
						Message:  fmt.Sprintf("Investigation complete: %s (%.0f%%)", inv.RootCause, inv.Confidence*100),
						Details:  inv.Conclusion, Severity: "info", CycleID: cycleID})
				})
				break
			}
		}
	}

	// Step 10.5: Causal chains enrich Issues with root cause code location
	if a.causalGraph != nil {
		chains := a.causalGraph.TraceAllAnomalies(snapshot)
		for _, ch := range chains {
			if ch.RootCause == "" {
				continue
			}
			// Match causal chain to issue by symptom metric name
			symptomMetric := strings.TrimPrefix(ch.Symptom, "metric:")
			for _, iss := range a.issueTracker.OpenIssues() {
				// Match by metric name in evidence or title
				matched := false
				for _, ev := range iss.Evidence {
					if strings.Contains(ev.Source, symptomMetric) {
						matched = true
						break
					}
				}
				if matched && (iss.CodeRefs == nil || len(iss.CodeRefs) == 0) {
					iss.CodeRefs = []string{ch.RootCause}
					a.logger.Printf("  CAUSAL: Issue %s → %s", iss.ID, ch.RootCause)
					break
				}
			}
		}
	}

	// Step 11: Dispatch notifications
	if len(toNotify) > 0 {
		a.logger.Printf("Dispatching %d notifications...", len(toNotify))
		errs := a.alertMgr.DispatchAll(ctx, toNotify)
		for _, err := range errs {
			a.logger.Printf("  Dispatch error: %v", err)
		}
		for _, al := range toNotify {
			a.emit(func(el *dashboard.EventLog) { el.AlertSent(cycleID, "push", al.Title) })
		}
	}

	// Step 12: Anomaly-triggered trace window
	if a.traceReceiver != nil && a.traceTargetAddr != "" && len(newIssues) > 0 {
		for _, iss := range newIssues {
			if iss.Severity >= types.SeverityCritical {
				a.logger.Printf("Triggering trace window (30s) for: %s", iss.Title)
				if err := a.traceReceiver.TriggerWindow(a.traceTargetAddr, 30*time.Second, "anomaly"); err != nil {
					a.logger.Printf("  Trace trigger failed: %v", err)
				}
				break
			}
		}
	}

	// Step 13: Persist issues to SQLite
	if a.db != nil {
		for _, iss := range a.issueTracker.OpenIssues() {
			a.db.SaveIssue(iss)
		}
	}

	// Step 14: Push everything to dashboard
	if a.store != nil {
		a.store.PushAgentHealth(&agentHealth)
		a.store.PushSnapshot(snapshot)
		a.store.PushIssues(a.issueTracker.OpenIssues(), nil)
		if a.traceReceiver != nil {
			if tw := a.traceReceiver.LatestWindow(); tw != nil {
				a.store.PushTrace(tw)
			}
		}
	}

	// Step 15: Cleanup old metric data (keep 7 days)
	if a.db != nil {
		a.db.CleanOldMetrics(7 * 24 * time.Hour)
	}

	// Finalize
	a.prevAlertCount = triggeredCount
	elapsed := time.Since(cycleStart).Seconds()
	a.logger.Printf("=== Cycle %s completed (%.1fs) ===", cycleID, elapsed)

	if triggeredCount == 0 && len(newIssues) == 0 {
		a.emit(func(el *dashboard.EventLog) { el.SystemOK(cycleID) })
	}
	a.emit(func(el *dashboard.EventLog) { el.CycleEnd(cycleID, elapsed) })

	return snapshot, nil
}

// emit safely sends an event to the activity feed (no-op if no dashboard).
func (a *Agent) emit(fn func(*dashboard.EventLog)) {
	if a.eventLog != nil {
		fn(a.eventLog)
	}
}

// SetAnalyst enables L2 AI analysis.
func (a *Agent) SetAnalyst(analyst *ai.Analyst) {
	a.analyst = analyst
}

// SetDashboard connects the agent to the dashboard for live data push.
func (a *Agent) SetDashboard(store *dashboard.Store, eventLog *dashboard.EventLog) {
	a.store = store
	a.eventLog = eventLog
}

// SetCausalGraph enables bidirectional causal chain tracing.
func (a *Agent) SetCausalGraph(g *causal.Graph) {
	a.causalGraph = g
}

// CausalGraph returns the causal graph (for API registration).
func (a *Agent) CausalGraph() *causal.Graph {
	return a.causalGraph
}

// SetCodeContext provides L0 scan results for AI analysis context.
func (a *Agent) SetCodeContext(ctx string) {
	a.codeContext = ctx
}

// SetTraceReceiver enables anomaly-triggered trace windows.
func (a *Agent) SetTraceReceiver(receiver *tracecollector.TraceReceiver, targetAddr string) {
	a.traceReceiver = receiver
	a.traceTargetAddr = targetAddr
}

// SetStorage enables SQLite persistence + baseline learning.
func (a *Agent) SetStorage(db *storage.DB) {
	a.db = db
	a.baselineLearner = storage.NewBaselineLearner(db, 48) // 24h at 30min intervals
}

// SetExpectationModel enables trace pattern learning.
func (a *Agent) SetExpectationModel(em *causal.ExpectationModel) {
	a.expectModel = em
}

// SetShadowVerifier enables independent data verification.
func (a *Agent) SetShadowVerifier(sv *causal.ShadowVerifier) {
	a.shadowVerifier = sv
}

// SetSyntheticRunner enables active health probes.
func (a *Agent) SetSyntheticRunner(sr *causal.SyntheticRunner) {
	a.syntheticRunner = sr
}

// SetInvestigator enables autonomous AI investigation on anomalies.
func (a *Agent) SetInvestigator(inv *investigator.Investigator) {
	a.autoInvestigator = inv
}

// Shutdown gracefully closes all resources.
func (a *Agent) Shutdown() {
	a.logger.Printf("Shutting down agent...")

	// Close collectors that implement io.Closer
	for _, c := range a.collectors.All() {
		if closer, ok := c.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				a.logger.Printf("  Close %s: %v", c.Name(), err)
			}
		}
	}

	// Stop trace receiver
	if a.traceReceiver != nil {
		a.traceReceiver.Stop()
	}

	a.logger.Printf("Agent shutdown complete.")
}

// PreviousAlertCount returns last cycle's triggered count (for AI gating).
func (a *Agent) PreviousAlertCount() int {
	return a.prevAlertCount
}

// IssueTracker returns the issue tracker (for AI gating checks).
func (a *Agent) IssueTracker() *issue.Tracker {
	return a.issueTracker
}

func issueToAlert(iss *types.Issue, source string) types.Alert {
	body := iss.Summary
	if iss.RootCause != "" {
		body += "\n根因: " + iss.RootCause
	}
	if len(iss.Suggestions) > 0 {
		body += "\n建议: " + iss.Suggestions[0]
	}
	if len(iss.Evidence) > 0 {
		body += fmt.Sprintf("\n证据: %s = %s", iss.Evidence[0].Source, iss.Evidence[0].Value)
	}

	return types.Alert{
		ID:        iss.ID,
		Severity:  iss.Severity,
		Title:     iss.Title,
		Body:      body,
		Source:    source,
		Labels:    map[string]string{"issue_id": iss.ID, "fingerprint": iss.Fingerprint},
		CreatedAt: time.Now(),
	}
}

func formatChainSteps(links []causal.ChainLink) string {
	var parts []string
	for _, l := range links {
		s := l.Description
		if l.File != "" {
			s += fmt.Sprintf(" (%s:%d)", l.File, l.Line)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n → ")
}

func collectAreas(result *ai.AnalysisResult) []string {
	seen := make(map[string]bool)
	var areas []string
	for _, a := range result.Anomalies {
		if a.Category != "" && !seen[a.Category] {
			seen[a.Category] = true
			areas = append(areas, a.Category)
		}
	}
	return areas
}
