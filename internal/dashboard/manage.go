package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ManageService handles instrument/uninstrument/uninstall operations via API.
type ManageService struct {
	mu         sync.Mutex
	agentBin   string // path to the agent binary
	sourcePath string // target project source path (if configured)
	dataDir    string // agent data directory
	eventLog   *EventLog
	aiInfo     AIInfo
}

// ManageStatus represents the current installation state.
type ManageStatus struct {
	AgentRunning     bool   `json:"agent_running"`
	AgentPID         int    `json:"agent_pid"`
	AgentUptime      string `json:"agent_uptime"`
	SourcePath       string `json:"source_path"`
	SourceConfigured bool   `json:"source_configured"`
	ProbesInstalled  bool   `json:"probes_installed"`
	ProbeFileCount   int    `json:"probe_file_count"`
	DataDir          string `json:"data_dir"`
	DataSizeMB       float64 `json:"data_size_mb"`
	Platform         string `json:"platform"`
	AIProvider       string `json:"ai_provider"`
	AIModel          string `json:"ai_model"`
	AIBaseURL        string `json:"ai_base_url"`
	AIEnabled        bool   `json:"ai_enabled"`
}

// TaskResult is returned by async operations.
type TaskResult struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Status    string    `json:"status"` // "running", "success", "failed"
	Output    string    `json:"output"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// AIInfo holds AI configuration for display in manage status.
type AIInfo struct {
	Provider string
	Model    string
	BaseURL  string
	Enabled  bool
}

func NewManageService(sourcePath, dataDir string, eventLog *EventLog) *ManageService {
	agentBin, _ := os.Executable()
	return &ManageService{
		agentBin:   agentBin,
		sourcePath: sourcePath,
		dataDir:    dataDir,
		eventLog:   eventLog,
	}
}

// SetAIInfo updates the AI configuration displayed in manage status.
func (m *ManageService) SetAIInfo(info AIInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.aiInfo = info
}

// GetStatus returns the current installation status.
func (m *ManageService) GetStatus() ManageStatus {
	status := ManageStatus{
		AgentRunning:     true, // if this API responds, agent is running
		AgentPID:         os.Getpid(),
		SourcePath:       m.sourcePath,
		SourceConfigured: m.sourcePath != "",
		Platform:         runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Check uptime
	status.AgentUptime = formatUptime(time.Since(startTime))

	// Check if probes are installed in source
	if m.sourcePath != "" {
		count := countInstrumentedFiles(m.sourcePath)
		status.ProbesInstalled = count > 0
		status.ProbeFileCount = count
	}

	// Data directory size
	status.DataDir = m.dataDir
	status.DataSizeMB = dirSizeMB(m.dataDir)

	// AI configuration
	m.mu.Lock()
	status.AIProvider = m.aiInfo.Provider
	status.AIModel = m.aiInfo.Model
	status.AIBaseURL = m.aiInfo.BaseURL
	status.AIEnabled = m.aiInfo.Enabled
	m.mu.Unlock()

	return status
}

// InstallProbes instruments the target project source code.
func (m *ManageService) InstallProbes(ctx context.Context) (*TaskResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.sourcePath == "" {
		return nil, fmt.Errorf("source path not configured")
	}

	result := &TaskResult{
		ID:        fmt.Sprintf("install-%d", time.Now().Unix()),
		Action:    "install_probes",
		Status:    "running",
		StartedAt: time.Now(),
	}

	// Auto-backup before instrument (safety net)
	bm := NewBackupManager("")
	if backup, err := bm.Backup(m.sourcePath); err != nil {
		m.logEvent("⚠️", "Backup failed: "+err.Error()+" — continuing anyway", "warning")
	} else {
		m.logEvent("💾", fmt.Sprintf("Auto-backup created: %s (%.1f MB)", backup.Path, backup.SizeMB), "info")
	}

	m.logEvent("🔧", "Installing SDK probes into: "+m.sourcePath, "info")

	cmd := exec.CommandContext(ctx, m.agentBin, "instrument", m.sourcePath+"/...")
	// If instrument is a separate binary:
	instrumentBin := filepath.Join(filepath.Dir(m.agentBin), "ai-ops-agent-instrument")
	if _, err := os.Stat(instrumentBin); err == nil {
		cmd = exec.CommandContext(ctx, instrumentBin, m.sourcePath+"/...")
	}

	output, err := cmd.CombinedOutput()
	now := time.Now()
	result.EndedAt = &now
	result.Output = string(output)

	if err != nil {
		result.Status = "failed"
		m.logEvent("❌", "Probe installation failed: "+err.Error(), "error")
		return result, nil
	}

	result.Status = "success"
	count := countInstrumentedFiles(m.sourcePath)
	m.logEvent("✅", fmt.Sprintf("Probes installed in %d files", count), "success")
	return result, nil
}

// StripProbes removes all instrumentation from the target project.
func (m *ManageService) StripProbes(ctx context.Context) (*TaskResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.sourcePath == "" {
		return nil, fmt.Errorf("source path not configured")
	}

	result := &TaskResult{
		ID:        fmt.Sprintf("strip-%d", time.Now().Unix()),
		Action:    "strip_probes",
		Status:    "running",
		StartedAt: time.Now(),
	}

	m.logEvent("🧹", "Stripping SDK probes from: "+m.sourcePath, "info")

	cmd := exec.CommandContext(ctx, m.agentBin, "instrument", "-strip", m.sourcePath+"/...")
	instrumentBin := filepath.Join(filepath.Dir(m.agentBin), "ai-ops-agent-instrument")
	if _, err := os.Stat(instrumentBin); err == nil {
		cmd = exec.CommandContext(ctx, instrumentBin, "-strip", m.sourcePath+"/...")
	}

	output, err := cmd.CombinedOutput()
	now := time.Now()
	result.EndedAt = &now
	result.Output = string(output)

	if err != nil {
		result.Status = "failed"
		m.logEvent("❌", "Probe stripping failed: "+err.Error(), "error")
		return result, nil
	}

	result.Status = "success"
	m.logEvent("✅", "All probes stripped. Source code restored.", "success")
	return result, nil
}

// TriggerTraceWindow sends a command to the SDK to start a trace window.
func (m *ManageService) TriggerTraceWindow(targetAddr string, durationSec int) error {
	if targetAddr == "" {
		targetAddr = "127.0.0.1:19877"
	}
	if durationSec <= 0 {
		durationSec = 10
	}
	if durationSec > 60 {
		durationSec = 60
	}

	m.logEvent("🔬", fmt.Sprintf("Triggering trace window (%ds) → %s", durationSec, targetAddr), "info")

	// Send UDP command to SDK
	cmd := fmt.Sprintf(`{"action":"start_window","duration":%d,"trigger":"manual"}`,
		durationSec*1000000000) // nanoseconds

	conn, err := net.DialTimeout("udp", targetAddr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach SDK at %s: %w", targetAddr, err)
	}
	defer conn.Close()
	_, err = conn.Write([]byte(cmd))
	return err
}

// CleanData removes the agent's data directory.
func (m *ManageService) CleanData() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.logEvent("🗑️", "Cleaning data directory: "+m.dataDir, "warning")
	if err := os.RemoveAll(m.dataDir); err != nil {
		return err
	}
	os.MkdirAll(m.dataDir, 0755) // recreate empty
	m.logEvent("✅", "Data directory cleaned.", "success")
	return nil
}

func (m *ManageService) logEvent(icon, msg, severity string) {
	if m.eventLog != nil {
		m.eventLog.Add(Event{
			Type: "manage", Icon: icon, Message: msg, Severity: severity,
		})
	}
}

// --- Helpers ---

var startTime = time.Now()

func countInstrumentedFiles(root string) int {
	count := 0
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), "__aiops_instrumented") {
			count++
		}
		return nil
	})
	return count
}

func dirSizeMB(path string) float64 {
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		size += info.Size()
		return nil
	})
	return float64(size) / 1024 / 1024
}

func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// --- HTTP Handlers (registered by Server) ---

func (s *Server) registerManageRoutes(mgr *ManageService) {
	// Helper: manage routes get CORS + auth
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return s.corsMiddleware(s.authMiddleware(h)) }

	s.mux.HandleFunc("/api/manage/status", wrap(func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, mgr.GetStatus())
	}))

	s.mux.HandleFunc("/api/manage/probes/install", wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		result, err := mgr.InstallProbes(ctx)
		if err != nil {
			s.writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, result)
	}))

	s.mux.HandleFunc("/api/manage/probes/strip", wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		result, err := mgr.StripProbes(ctx)
		if err != nil {
			s.writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, result)
	}))

	s.mux.HandleFunc("/api/manage/trace/trigger", wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			TargetAddr  string `json:"target_addr"`
			DurationSec int    `json:"duration_sec"`
		}
		json.Unmarshal(body, &req)
		if err := mgr.TriggerTraceWindow(req.TargetAddr, req.DurationSec); err != nil {
			s.writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, map[string]string{"status": "ok"})
	}))

	s.mux.HandleFunc("/api/manage/data/clean", wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if err := mgr.CleanData(); err != nil {
			s.writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, map[string]string{"status": "ok"})
	}))
}
