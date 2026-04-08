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
	"strings"
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
	authToken  string // if set, all API requests must include ?token=xxx or Authorization: Bearer xxx
	mux        *http.ServeMux
}

// CausalGraphRegistrar allows the causal package to register its API routes.
type CausalGraphRegistrar interface {
	RegisterToMux(mux *http.ServeMux, cors func(http.HandlerFunc) http.HandlerFunc)
}

func NewServer(store *Store, addr string, eventLog *EventLog, chat *ChatService, mgr *ManageService, authToken string) *Server {
	if addr == "" {
		addr = "127.0.0.1:9090" // bind to localhost by default for security
	}
	s := &Server{store: store, addr: addr, eventLog: eventLog, chat: chat, authToken: authToken, mux: http.NewServeMux()}
	s.registerRoutes()
	if mgr != nil {
		s.registerManageRoutes(mgr)
	}
	return s
}

// authMiddleware checks token if authToken is configured.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authToken != "" {
			token := r.URL.Query().Get("token")
			if token == "" {
				auth := r.Header.Get("Authorization")
				if strings.HasPrefix(auth, "Bearer ") {
					token = auth[7:]
				}
			}
			if token != s.authToken {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// SetAITerminal enables the interactive AI terminal.
func (s *Server) SetAITerminal(terminal *AITerminal) {
	s.aiTerminal = terminal
	s.mux.HandleFunc("/api/terminal/ask", s.apiWrap(s.handleTerminalAsk))
	s.mux.HandleFunc("/api/terminal/session", s.apiWrap(s.handleTerminalSession))
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
	registrar.RegisterToMux(s.mux, s.apiWrap)
}

// Mux returns the HTTP mux for external route registration.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// APIWrap returns the CORS+auth middleware for external route registration.
func (s *Server) APIWrap() func(http.HandlerFunc) http.HandlerFunc { return s.apiWrap }

func (s *Server) Start() error {
	log.Printf("[dashboard] Starting at http://localhost%s", s.addr)
	return http.ListenAndServe(s.addr, s.mux)
}

// apiWrap wraps ALL API handlers with CORS + auth. Single security boundary.
func (s *Server) apiWrap(h http.HandlerFunc) http.HandlerFunc {
	return s.corsMiddleware(s.authMiddleware(h))
}

func (s *Server) registerRoutes() {
	// ALL API endpoints go through CORS + auth
	w := s.apiWrap
	s.mux.HandleFunc("/api/overview", w(s.handleOverview))
	s.mux.HandleFunc("/api/metrics", w(s.handleMetrics))
	s.mux.HandleFunc("/api/metrics/series", w(s.handleMetricSeries))
	s.mux.HandleFunc("/api/metrics/names", w(s.handleMetricNames))
	s.mux.HandleFunc("/api/metrics/annotations", w(s.handleMetricAnnotations))
	s.mux.HandleFunc("/api/issues", w(s.handleIssues))
	s.mux.HandleFunc("/api/issues/closed", w(s.handleClosedIssues))
	s.mux.HandleFunc("/api/rules", w(s.handleRules))
	s.mux.HandleFunc("/api/analysis", w(s.handleAnalysis))
	s.mux.HandleFunc("/api/trace", w(s.handleTrace))
	s.mux.HandleFunc("/api/logs", w(s.handleLogs))

	// Request traces
	s.mux.HandleFunc("/api/traces/recent", w(s.handleRecentTraces))

	// Activity feed + Chat
	s.mux.HandleFunc("/api/events", w(s.handleEvents))
	s.mux.HandleFunc("/api/events/poll", w(s.handleEventsPoll))
	s.mux.HandleFunc("/api/chat", w(s.handleChat))
	s.mux.HandleFunc("/api/chat/history", w(s.handleChatHistory))

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
	annotations := AnnotateMetrics(metrics, s.store.GetBaseline())
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
