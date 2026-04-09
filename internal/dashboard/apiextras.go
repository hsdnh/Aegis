// Extra API endpoints for features that were built but not exposed to the dashboard.
package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/hsdnh/Aegis/internal/causal"
	"github.com/hsdnh/Aegis/internal/changefeed"
	"github.com/hsdnh/Aegis/internal/cluster"
	"github.com/hsdnh/Aegis/internal/healthcheck"
	"github.com/hsdnh/Aegis/internal/investigator"
	"github.com/hsdnh/Aegis/internal/issue"
	"github.com/hsdnh/Aegis/internal/runbook"
	"github.com/hsdnh/Aegis/internal/storage"
	"github.com/hsdnh/Aegis/sdk/probe"
)

// ExtraServices holds references to all subsystems for API registration.
type ExtraServices struct {
	SilenceManager  *issue.SilenceManager
	RunbookRegistry *runbook.Registry
	Investigator    *investigator.Investigator
	ShadowVerifier  *causal.ShadowVerifier
	SyntheticRunner *causal.SyntheticRunner
	NodeRegistry    *cluster.NodeRegistry
	ChangeFeed      *changefeed.Feed
	BaselineLearner *storage.BaselineLearner
	PreflightReport interface{} // *healthcheck.PreflightReport stored as interface
}

// RegisterExtraRoutes adds all missing API endpoints.
func (s *Server) RegisterExtraRoutes(extras ExtraServices) {
	w := s.apiWrap

	// --- Silence / maintenance windows ---
	if extras.SilenceManager != nil {
		sm := extras.SilenceManager
		s.mux.HandleFunc("/api/silence/list", w(func(rw http.ResponseWriter, r *http.Request) {
			s.writeJSON(rw, map[string]interface{}{
				"active": sm.ActiveRules(),
				"all":    sm.AllRules(),
			})
		}))
		s.mux.HandleFunc("/api/silence/create", w(func(rw http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" { http.Error(rw, "POST", 405); return }
			body, _ := io.ReadAll(r.Body)
			var rule issue.SilenceRule
			json.Unmarshal(body, &rule)
			if rule.Reason == "" { rule.Reason = "manual" }
			if rule.EndTime.IsZero() { rule.EndTime = time.Now().Add(2 * time.Hour) }
			if rule.StartTime.IsZero() { rule.StartTime = time.Now() }
			sm.AddRule(rule)
			s.writeJSON(rw, map[string]string{"status": "ok", "id": rule.ID})
		}))
		s.mux.HandleFunc("/api/silence/delete", w(func(rw http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" { http.Error(rw, "POST", 405); return }
			body, _ := io.ReadAll(r.Body)
			var req struct{ ID string `json:"id"` }
			json.Unmarshal(body, &req)
			sm.RemoveRule(req.ID)
			s.writeJSON(rw, map[string]string{"status": "ok"})
		}))
	}

	// --- Runbooks ---
	if extras.RunbookRegistry != nil {
		rb := extras.RunbookRegistry
		s.mux.HandleFunc("/api/runbooks", w(func(rw http.ResponseWriter, r *http.Request) {
			s.writeJSON(rw, rb.AllRunbooks())
		}))
		s.mux.HandleFunc("/api/runbooks/match", w(func(rw http.ResponseWriter, r *http.Request) {
			rule := r.URL.Query().Get("rule")
			title := r.URL.Query().Get("title")
			summary := r.URL.Query().Get("summary")
			s.writeJSON(rw, rb.MatchIssue(rule, title, summary))
		}))
		s.mux.HandleFunc("/api/runbooks/execute", w(func(rw http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" { http.Error(rw, "POST", 405); return }
			body, _ := io.ReadAll(r.Body)
			var req struct {
				RunbookID  string `json:"runbook_id"`
				ActionName string `json:"action_name"`
			}
			json.Unmarshal(body, &req)
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			result := rb.Execute(ctx, req.RunbookID, req.ActionName)
			s.writeJSON(rw, result)
		}))
	}

	// --- Investigator ---
	if extras.Investigator != nil {
		inv := extras.Investigator
		s.mux.HandleFunc("/api/investigations", w(func(rw http.ResponseWriter, r *http.Request) {
			s.writeJSON(rw, inv.RecentInvestigations(20))
		}))
		s.mux.HandleFunc("/api/investigations/run", w(func(rw http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" { http.Error(rw, "POST", 405); return }
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Anomaly  string   `json:"anomaly"`
				Evidence []string `json:"evidence"`
			}
			json.Unmarshal(body, &req)
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			defer cancel()
			result := inv.Investigate(ctx, req.Anomaly, req.Evidence)
			s.writeJSON(rw, result)
		}))
	}

	// --- Shadow verification ---
	if extras.ShadowVerifier != nil {
		sv := extras.ShadowVerifier
		s.mux.HandleFunc("/api/shadow/results", w(func(rw http.ResponseWriter, r *http.Request) {
			s.writeJSON(rw, map[string]interface{}{
				"results":    sv.RecentResults(20),
				"checks":     sv.AllChecks(),
				"fail_count": sv.FailCount(),
			})
		}))
	}

	// --- Synthetic probes ---
	if extras.SyntheticRunner != nil {
		sr := extras.SyntheticRunner
		s.mux.HandleFunc("/api/probes/summary", w(func(rw http.ResponseWriter, r *http.Request) {
			s.writeJSON(rw, sr.Summary())
		}))
		s.mux.HandleFunc("/api/probes/results", w(func(rw http.ResponseWriter, r *http.Request) {
			s.writeJSON(rw, sr.RecentResults(20))
		}))
		s.mux.HandleFunc("/api/probes/run", w(func(rw http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" { http.Error(rw, "POST", 405); return }
			body, _ := io.ReadAll(r.Body)
			var req struct { Name string `json:"name"` }
			json.Unmarshal(body, &req)
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			result := sr.RunOne(ctx, req.Name)
			s.writeJSON(rw, result)
		}))
	}

	// --- Cluster nodes ---
	if extras.NodeRegistry != nil {
		nr := extras.NodeRegistry
		s.mux.HandleFunc("/api/cluster/nodes", w(func(rw http.ResponseWriter, r *http.Request) {
			nodes := nr.AllNodes()
			s.writeJSON(rw, map[string]interface{}{
				"nodes":        nodes,
				"total":        len(nodes),
				"online_count": nr.OnlineCount(),
			})
		}))
		s.mux.HandleFunc("/api/cluster/node", w(func(rw http.ResponseWriter, r *http.Request) {
			id := r.URL.Query().Get("id")
			s.writeJSON(rw, nr.GetNode(id))
		}))
	}

	// --- Change feed ---
	if extras.ChangeFeed != nil {
		cf := extras.ChangeFeed
		s.mux.HandleFunc("/api/changes/recent", w(func(rw http.ResponseWriter, r *http.Request) {
			s.writeJSON(rw, cf.Recent(20))
		}))
		s.mux.HandleFunc("/api/changes/near", w(func(rw http.ResponseWriter, r *http.Request) {
			ts := r.URL.Query().Get("time")
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil { t = time.Now() }
			s.writeJSON(rw, cf.NearIssue(t, 30*time.Minute, 10*time.Minute))
		}))
	}

	// --- Baseline learning status ---
	if extras.BaselineLearner != nil {
		bl := extras.BaselineLearner
		s.mux.HandleFunc("/api/baseline/status", w(func(rw http.ResponseWriter, r *http.Request) {
			s.writeJSON(rw, map[string]interface{}{
				"ready":    bl.IsReady(),
				"baseline": bl.GetAIBaseline(),
			})
		}))
	}

	// --- Checkpoint violations ---
	s.mux.HandleFunc("/api/checkpoints/violations", w(func(rw http.ResponseWriter, r *http.Request) {
		s.writeJSON(rw, map[string]interface{}{
			"violations": probe.GlobalCheckpoint.RecentViolations(20),
			"stats":      probe.GlobalCheckpoint.Stats(),
		})
	}))

	// --- Preflight report ---
	s.mux.HandleFunc("/api/preflight", w(func(rw http.ResponseWriter, r *http.Request) {
		if extras.PreflightReport != nil {
			s.writeJSON(rw, extras.PreflightReport)
		} else {
			s.writeJSON(rw, map[string]string{"status": "not_run"})
		}
	}))

	// --- Auto-detect projects ---
	s.mux.HandleFunc("/api/detect/projects", w(func(rw http.ResponseWriter, r *http.Request) {
		projects := healthcheck.DetectProjects()
		s.writeJSON(rw, projects)
	}))

	// --- Auto-extract config from detected project ---
	s.mux.HandleFunc("/api/detect/config", w(func(rw http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			// Auto-detect first project
			projects := healthcheck.DetectProjects()
			if len(projects) > 0 {
				path = projects[0].Path
			}
		}
		if path == "" {
			s.writeJSON(rw, map[string]string{"error": "no project found"})
			return
		}
		extracted := healthcheck.AutoExtractConfig(path)
		s.writeJSON(rw, map[string]interface{}{
			"extracted": extracted,
			"yaml":      extracted.ToYAML(),
		})
	}))
}
