package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// MasterReceiver accepts reports from worker nodes via HTTP.
type MasterReceiver struct {
	registry  *NodeRegistry
	authToken string
	mux       *http.ServeMux
}

func NewMasterReceiver(registry *NodeRegistry) *MasterReceiver {
	return &MasterReceiver{
		registry:  registry,
		authToken: os.Getenv("AIOPS_CLUSTER_TOKEN"),
		mux:       http.NewServeMux(),
	}
}

// RegisterRoutes adds cluster API endpoints to a mux.
// Called by the dashboard server to share the same port.
func (m *MasterReceiver) RegisterRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/api/cluster/report", m.authCheck(m.handleReport))
	mux.HandleFunc("/api/cluster/nodes", wrap(m.handleNodes))
	mux.HandleFunc("/api/cluster/node", wrap(m.handleNodeDetail))
}

// Start runs the cluster receiver on a separate port (for non-dashboard setups).
func (m *MasterReceiver) Start(addr string) error {
	m.mux.HandleFunc("/api/cluster/report", m.authCheck(m.handleReport))
	log.Printf("[cluster] Master receiver listening on %s", addr)
	return http.ListenAndServe(addr, m.mux)
}

func (m *MasterReceiver) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	var report NodeReport
	if err := json.Unmarshal(body, &report); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	m.registry.Update(report)

	log.Printf("[cluster] Received report from %s: %d metrics, %d rules, %d issues",
		report.Node.Name, len(report.MetricSummary), len(report.TriggeredRules), len(report.Issues))

	// Check for critical issues on the worker and log them
	for _, iss := range report.Issues {
		if iss.Severity >= 2 { // CRITICAL or FATAL
			log.Printf("[cluster] ALERT from %s: [%s] %s", report.Node.Name, iss.Severity, iss.Title)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (m *MasterReceiver) handleNodes(w http.ResponseWriter, r *http.Request) {
	nodes := m.registry.AllNodes()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"nodes":        nodes,
		"total":        len(nodes),
		"online_count": m.registry.OnlineCount(),
	})
}

func (m *MasterReceiver) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	nodeID := r.URL.Query().Get("id")
	if nodeID == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "id parameter required"})
		return
	}

	node := m.registry.GetNode(nodeID)
	if node == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "node not found"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(node)
}

func (m *MasterReceiver) authCheck(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.authToken != "" {
			auth := r.Header.Get("Authorization")
			token := ""
			if strings.HasPrefix(auth, "Bearer ") {
				token = auth[7:]
			}
			if token == "" {
				token = r.URL.Query().Get("token")
			}
			if token != m.authToken {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// ClusterSummary returns a cross-node analysis summary.
type ClusterSummary struct {
	TotalNodes    int           `json:"total_nodes"`
	OnlineNodes   int           `json:"online_nodes"`
	TotalIssues   int           `json:"total_issues"`
	CriticalNodes []string      `json:"critical_nodes"` // nodes with CRITICAL+ issues
	NodeSummaries []NodeSummary `json:"node_summaries"`
}

type NodeSummary struct {
	NodeName      string  `json:"node_name"`
	Online        bool    `json:"online"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryMB      int     `json:"memory_mb"`
	IssueCount    int     `json:"issue_count"`
	RuleTriggered int     `json:"rule_triggered"`
	ErrorLogs     int     `json:"error_logs"`
	HealthScore   float64 `json:"health_score"` // 0-100
}

// Summary generates a cross-node overview.
func (m *MasterReceiver) Summary() ClusterSummary {
	nodes := m.registry.AllNodes()
	summary := ClusterSummary{
		TotalNodes:  len(nodes),
		OnlineNodes: m.registry.OnlineCount(),
	}

	for _, n := range nodes {
		ns := NodeSummary{
			NodeName: n.Info.Name,
			Online:   n.Online,
		}

		if n.LastReport != nil {
			r := n.LastReport
			ns.CPUPercent = r.CPUPercent
			ns.MemoryMB = r.MemoryMB
			ns.IssueCount = len(r.Issues)
			ns.RuleTriggered = len(r.TriggeredRules)
			ns.ErrorLogs = r.ErrorLogCount
			summary.TotalIssues += len(r.Issues)

			// Health score: 100 - penalties
			score := 100.0
			if !n.Online {
				score = 0
			} else {
				score -= float64(len(r.TriggeredRules)) * 10
				score -= float64(r.ErrorLogCount) * 0.5
				if r.CPUPercent > 80 {
					score -= 20
				}
				if score < 0 {
					score = 0
				}
			}
			ns.HealthScore = score

			for _, iss := range r.Issues {
				if iss.Severity >= 2 {
					summary.CriticalNodes = append(summary.CriticalNodes, n.Info.Name)
					break
				}
			}
		}

		summary.NodeSummaries = append(summary.NodeSummaries, ns)
	}

	return summary
}

// FormatForAI returns a text summary for AI cross-node analysis.
func (m *MasterReceiver) FormatForAI() string {
	summary := m.Summary()
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Cluster Status: %d/%d nodes online\n\n",
		summary.OnlineNodes, summary.TotalNodes))

	for _, ns := range summary.NodeSummaries {
		status := "🟢"
		if !ns.Online {
			status = "🔴 OFFLINE"
		} else if ns.HealthScore < 50 {
			status = "🔴"
		} else if ns.HealthScore < 80 {
			status = "🟡"
		}

		sb.WriteString(fmt.Sprintf("### %s %s (score: %.0f)\n", status, ns.NodeName, ns.HealthScore))
		sb.WriteString(fmt.Sprintf("  CPU: %.0f%%, Memory: %dMB, Issues: %d, Rules: %d, Errors: %d\n\n",
			ns.CPUPercent, ns.MemoryMB, ns.IssueCount, ns.RuleTriggered, ns.ErrorLogs))
	}

	if len(summary.CriticalNodes) > 0 {
		sb.WriteString(fmt.Sprintf("⚠️ Critical nodes: %s\n", strings.Join(summary.CriticalNodes, ", ")))
	}

	return sb.String()
}
