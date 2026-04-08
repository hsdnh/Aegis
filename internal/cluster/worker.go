package cluster

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/hsdnh/Aegis/pkg/types"
)

// WorkerReporter sends monitoring results to the master node.
type WorkerReporter struct {
	masterURL string // e.g. "http://10.0.0.1:19800"
	nodeInfo  NodeInfo
	client    *http.Client
	authToken string
}

func NewWorkerReporter(masterURL string, nodeInfo NodeInfo) *WorkerReporter {
	return &WorkerReporter{
		masterURL: masterURL,
		nodeInfo:  nodeInfo,
		client:    &http.Client{Timeout: 10 * time.Second},
		authToken: os.Getenv("AIOPS_CLUSTER_TOKEN"),
	}
}

// BuildReport creates a NodeReport from the current cycle's snapshot.
func (w *WorkerReporter) BuildReport(snapshot *types.Snapshot, issues []*types.Issue, agentHealth types.AgentHealth) NodeReport {
	w.nodeInfo.LastSeen = time.Now()

	report := NodeReport{
		Node:        w.nodeInfo,
		CycleID:     snapshot.CycleID,
		Timestamp:   time.Now(),
		Health:      snapshot.Health,
		AgentHealth: agentHealth,
		Issues:      issues,
	}

	// Summarize metrics
	for _, r := range snapshot.Results {
		for _, m := range r.Metrics {
			status := "ok"
			report.MetricSummary = append(report.MetricSummary, MetricSummary{
				Name: m.Name, Value: m.Value, Status: status,
			})
		}
	}

	// Triggered rules only
	for _, rr := range snapshot.RuleResults {
		if rr.Triggered {
			report.TriggeredRules = append(report.TriggeredRules, rr)
		}
	}

	// Error log summary
	var topErrors []string
	for _, r := range snapshot.Results {
		for _, l := range r.Logs {
			if l.Level == "ERROR" {
				report.ErrorLogCount++
				if len(topErrors) < 10 {
					msg := l.Message
					if len(msg) > 200 {
						msg = msg[:200]
					}
					topErrors = append(topErrors, msg)
				}
			}
		}
	}
	report.TopErrors = topErrors

	// System resources
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	report.MemoryMB = int(memStats.Alloc / 1024 / 1024)
	report.Goroutines = runtime.NumGoroutine()

	return report
}

// Send transmits the report to the master.
func (w *WorkerReporter) Send(report NodeReport) error {
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}

	url := w.masterURL + "/api/cluster/report"
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if w.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.authToken)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("send to master: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("master returned %d", resp.StatusCode)
	}

	log.Printf("[cluster] Reported to master: %d metrics, %d rules, %d issues",
		len(report.MetricSummary), len(report.TriggeredRules), len(report.Issues))
	return nil
}
