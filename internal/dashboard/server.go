package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"time"
)

//go:embed static
var staticFiles embed.FS

// Server serves the dashboard web UI and API.
type Server struct {
	store      *Store
	eventLog   *EventLog
	chat       *ChatService
	aiTerminal *AITerminal
	addr       string
	mux      *http.ServeMux
}

// CausalGraphRegistrar allows the causal package to register its API routes.
type CausalGraphRegistrar interface {
	RegisterToMux(mux *http.ServeMux, cors func(http.HandlerFunc) http.HandlerFunc)
}

func NewServer(store *Store, addr string, eventLog *EventLog, chat *ChatService, mgr *ManageService) *Server {
	if addr == "" {
		addr = ":9090"
	}
	s := &Server{store: store, addr: addr, eventLog: eventLog, chat: chat, mux: http.NewServeMux()}
	s.registerRoutes()
	if mgr != nil {
		s.registerManageRoutes(mgr)
	}
	return s
}

// SetAITerminal enables the interactive AI terminal.
func (s *Server) SetAITerminal(terminal *AITerminal) {
	s.aiTerminal = terminal
	s.mux.HandleFunc("/api/terminal/ask", s.corsMiddleware(s.handleTerminalAsk))
	s.mux.HandleFunc("/api/terminal/session", s.corsMiddleware(s.handleTerminalSession))
}

func (s *Server) handleTerminalAsk(w http.ResponseWriter, r *http.Request) {
	if s.aiTerminal == nil {
		s.writeJSON(w, map[string]string{"error": "AI terminal not configured"})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
	}
	json.Unmarshal(body, &req)
	if req.Message == "" {
		s.writeJSON(w, map[string]string{"error": "message required"})
		return
	}
	if req.SessionID == "" {
		req.SessionID = "default"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	msg, err := s.aiTerminal.Ask(ctx, req.SessionID, req.Message)
	if err != nil {
		s.writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, msg)
}

func (s *Server) handleTerminalSession(w http.ResponseWriter, r *http.Request) {
	if s.aiTerminal == nil {
		s.writeJSON(w, map[string]string{"error": "AI terminal not configured"})
		return
	}
	sid := r.URL.Query().Get("id")
	if sid == "" {
		sid = "default"
	}
	session := s.aiTerminal.GetSession(sid)
	if session == nil {
		s.writeJSON(w, []interface{}{})
		return
	}
	s.writeJSON(w, session)
}

// RegisterCausalAPI adds causal chain endpoints. Called after server creation.
func (s *Server) RegisterCausalAPI(registrar CausalGraphRegistrar) {
	registrar.RegisterToMux(s.mux, s.corsMiddleware)
}

func (s *Server) Start() error {
	log.Printf("[dashboard] Starting at http://localhost%s", s.addr)
	return http.ListenAndServe(s.addr, s.mux)
}

func (s *Server) registerRoutes() {
	// Existing API endpoints (all wrapped with CORS)
	s.mux.HandleFunc("/api/overview", s.corsMiddleware(s.handleOverview))
	s.mux.HandleFunc("/api/metrics", s.corsMiddleware(s.handleMetrics))
	s.mux.HandleFunc("/api/metrics/series", s.corsMiddleware(s.handleMetricSeries))
	s.mux.HandleFunc("/api/metrics/names", s.corsMiddleware(s.handleMetricNames))
	s.mux.HandleFunc("/api/metrics/annotations", s.corsMiddleware(s.handleMetricAnnotations))
	s.mux.HandleFunc("/api/issues", s.corsMiddleware(s.handleIssues))
	s.mux.HandleFunc("/api/issues/closed", s.corsMiddleware(s.handleClosedIssues))
	s.mux.HandleFunc("/api/rules", s.corsMiddleware(s.handleRules))
	s.mux.HandleFunc("/api/analysis", s.corsMiddleware(s.handleAnalysis))
	s.mux.HandleFunc("/api/trace", s.corsMiddleware(s.handleTrace))
	s.mux.HandleFunc("/api/logs", s.corsMiddleware(s.handleLogs))

	// Request traces (for waterfall view)
	s.mux.HandleFunc("/api/traces/recent", s.corsMiddleware(s.handleRecentTraces))

	// Activity feed + Chat
	s.mux.HandleFunc("/api/events", s.corsMiddleware(s.handleEvents))
	s.mux.HandleFunc("/api/events/poll", s.corsMiddleware(s.handleEventsPoll))
	s.mux.HandleFunc("/api/chat", s.corsMiddleware(s.handleChat))
	s.mux.HandleFunc("/api/chat/history", s.corsMiddleware(s.handleChatHistory))

	// Static files (embedded)
	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("[dashboard] embed error: %v", err)
	}
	s.mux.Handle("/", http.FileServer(http.FS(staticSub)))
}

func (s *Server) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// --- Existing handlers ---

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.store.GetOverview())
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.store.GetMetrics())
}

func (s *Server) handleMetricSeries(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		s.writeJSON(w, map[string]string{"error": "name parameter required"})
		return
	}
	s.writeJSON(w, s.store.GetMetricSeries(name))
}

func (s *Server) handleMetricNames(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.store.GetAllMetricNames())
}

func (s *Server) handleMetricAnnotations(w http.ResponseWriter, r *http.Request) {
	metrics := s.store.GetMetrics()
	annotations := AnnotateMetrics(metrics, nil) // TODO: pass baseline
	s.writeJSON(w, annotations)
}

func (s *Server) handleRecentTraces(w http.ResponseWriter, r *http.Request) {
	// Returns empty array — trace store is populated by the SDK probe.
	// When a TraceStore is connected, it returns real data.
	s.writeJSON(w, []interface{}{})
}

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	issues := s.store.GetIssues()
	if issues == nil {
		s.writeJSON(w, []interface{}{})
		return
	}
	s.writeJSON(w, issues)
}

func (s *Server) handleClosedIssues(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.store.GetClosedIssues())
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.store.GetRuleResults())
}

func (s *Server) handleAnalysis(w http.ResponseWriter, r *http.Request) {
	analysis := s.store.GetLatestAnalysis()
	if analysis == nil {
		s.writeJSON(w, map[string]string{"status": "no_analysis"})
		return
	}
	s.writeJSON(w, analysis)
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	trace := s.store.GetLatestTrace()
	if trace == nil {
		s.writeJSON(w, map[string]string{"status": "no_trace"})
		return
	}
	s.writeJSON(w, trace)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.store.GetLogs())
}

// --- New: Activity Feed ---

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.eventLog == nil {
		s.writeJSON(w, []interface{}{})
		return
	}
	n := 50
	if qn := r.URL.Query().Get("n"); qn != "" {
		if parsed, err := strconv.Atoi(qn); err == nil && parsed > 0 {
			n = parsed
		}
	}
	s.writeJSON(w, s.eventLog.Recent(n))
}

// handleEventsPoll supports long-polling: returns new events since `after` ID.
func (s *Server) handleEventsPoll(w http.ResponseWriter, r *http.Request) {
	if s.eventLog == nil {
		s.writeJSON(w, []interface{}{})
		return
	}
	afterID := 0
	if qid := r.URL.Query().Get("after"); qid != "" {
		afterID, _ = strconv.Atoi(qid)
	}

	// Quick check first
	events := s.eventLog.Since(afterID)
	if len(events) > 0 {
		s.writeJSON(w, events)
		return
	}

	// Wait up to 10 seconds for new events
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.writeJSON(w, []interface{}{})
			return
		case <-ticker.C:
			events = s.eventLog.Since(afterID)
			if len(events) > 0 {
				s.writeJSON(w, events)
				return
			}
		}
	}
}

// --- New: AI Chat ---

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if s.chat == nil {
		s.writeJSON(w, map[string]string{"error": "AI chat not configured (set ai.enabled=true)"})
		return
	}

	if r.Method != "POST" {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSON(w, map[string]string{"error": "read body failed"})
		return
	}

	var req struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Message == "" {
		s.writeJSON(w, map[string]string{"error": "message field required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	response, err := s.chat.Ask(ctx, req.Message)
	if err != nil {
		s.writeJSON(w, map[string]string{"error": err.Error()})
		return
	}

	s.writeJSON(w, map[string]string{"response": response})
}

func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	if s.chat == nil {
		s.writeJSON(w, []interface{}{})
		return
	}
	s.writeJSON(w, s.chat.History())
}
