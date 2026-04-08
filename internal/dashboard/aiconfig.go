// AI model configuration API — switch models from the dashboard without restarting.
package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/hsdnh/Aegis/internal/ai"
)

// AIConfigManager allows hot-switching AI models from the dashboard.
type AIConfigManager struct {
	mu            sync.RWMutex
	currentConfig AIModelInfo
	presets       []AIModelPreset
	onSwitch      func(client *ai.Client) // callback to update the agent's AI client
}

// AIModelInfo describes the currently active AI model.
type AIModelInfo struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key_masked"` // masked for display
	Enabled  bool   `json:"enabled"`
}

// AIModelPreset is a pre-configured model option shown in the dashboard dropdown.
type AIModelPreset struct {
	Name     string `json:"name"`     // display name
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"` // can be empty if user provides
}

func NewAIConfigManager(provider, model, baseURL, apiKey string, enabled bool) *AIConfigManager {
	masked := ""
	if len(apiKey) > 8 {
		masked = apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
	}

	mgr := &AIConfigManager{
		currentConfig: AIModelInfo{
			Provider: provider,
			Model:    model,
			BaseURL:  baseURL,
			APIKey:   masked,
			Enabled:  enabled,
		},
		presets: defaultPresets(),
	}
	return mgr
}

// SetSwitchCallback sets the function called when user switches model.
func (m *AIConfigManager) SetSwitchCallback(fn func(client *ai.Client)) {
	m.onSwitch = fn
}

// RegisterRoutes adds AI config endpoints to the dashboard.
func (m *AIConfigManager) RegisterRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	// Get current config + presets
	mux.HandleFunc("/api/ai/config", wrap(func(w http.ResponseWriter, r *http.Request) {
		m.mu.RLock()
		defer m.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"current": m.currentConfig,
			"presets": m.presets,
		})
	}))

	// Switch model
	mux.HandleFunc("/api/ai/switch", wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST", 405)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			BaseURL  string `json:"base_url"`
			APIKey   string `json:"api_key"`
		}
		json.Unmarshal(body, &req)

		if req.Provider == "" || req.Model == "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "provider and model required"})
			return
		}

		// Create new client
		apiKey := req.APIKey
		if apiKey == "" {
			// Keep existing key if not provided
			m.mu.RLock()
			// Can't retrieve masked key — user must provide
			m.mu.RUnlock()
		}

		newClient := ai.NewClientWithURL(ai.Provider(req.Provider), apiKey, req.Model, req.BaseURL)

		// Update state
		m.mu.Lock()
		masked := ""
		if len(apiKey) > 8 {
			masked = apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
		}
		m.currentConfig = AIModelInfo{
			Provider: req.Provider,
			Model:    req.Model,
			BaseURL:  req.BaseURL,
			APIKey:   masked,
			Enabled:  true,
		}
		m.mu.Unlock()

		// Callback to swap client in agent
		if m.onSwitch != nil {
			m.onSwitch(newClient)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "model": req.Model})
	}))
}

func defaultPresets() []AIModelPreset {
	return []AIModelPreset{
		{Name: "Claude Opus 4 (最强)", Provider: "claude", Model: "claude-opus-4-20250514"},
		{Name: "Claude Sonnet 4 (性价比)", Provider: "claude", Model: "claude-sonnet-4-20250514"},
		{Name: "GPT-4o", Provider: "openai", Model: "gpt-4o"},
		{Name: "o3 (推理最强)", Provider: "openai", Model: "o3-2025-04-16"},
		{Name: "DeepSeek V3", Provider: "openai_compatible", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1"},
		{Name: "DeepSeek R1 (推理)", Provider: "openai_compatible", Model: "deepseek-reasoner", BaseURL: "https://api.deepseek.com/v1"},
		{Name: "通义千问 Max", Provider: "openai_compatible", Model: "qwen-max", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{Name: "通义千问 Turbo (快)", Provider: "openai_compatible", Model: "qwen-turbo", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{Name: "Ollama 本地 (免费)", Provider: "openai_compatible", Model: "llama3", BaseURL: "http://localhost:11434/v1"},
		{Name: "自定义...", Provider: "openai_compatible", Model: "", BaseURL: ""},
	}
}
