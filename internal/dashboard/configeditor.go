// Config editor: edit config.yaml from the dashboard, hot-reload on save.
package dashboard

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// ConfigEditor allows viewing and editing config.yaml from the dashboard.
type ConfigEditor struct {
	mu         sync.Mutex
	configPath string
	onReload   func() error // callback to reload config in the agent
}

func NewConfigEditor(configPath string, onReload func() error) *ConfigEditor {
	return &ConfigEditor{
		configPath: configPath,
		onReload:   onReload,
	}
}

// RegisterRoutes adds config editor endpoints.
func (ce *ConfigEditor) RegisterRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	// Read config
	mux.HandleFunc("/api/config/read", wrap(func(w http.ResponseWriter, r *http.Request) {
		ce.mu.Lock()
		defer ce.mu.Unlock()

		data, err := os.ReadFile(ce.configPath)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "读取配置失败: " + err.Error()})
			return
		}

		info, _ := os.Stat(ce.configPath)
		var modTime string
		if info != nil {
			modTime = info.ModTime().Format("2006-01-02 15:04:05")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"content":   string(data),
			"path":      ce.configPath,
			"modified":  modTime,
			"size":      len(data),
		})
	}))

	// Save config
	mux.HandleFunc("/api/config/save", wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}

		ce.mu.Lock()
		defer ce.mu.Unlock()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "读取请求失败"})
			return
		}

		var req struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Content == "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "内容不能为空"})
			return
		}

		// Backup current config
		backupPath := ce.configPath + ".bak." + time.Now().Format("20060102-150405")
		if data, err := os.ReadFile(ce.configPath); err == nil {
			os.WriteFile(backupPath, data, 0644)
		}

		// Write new config
		if err := os.WriteFile(ce.configPath, []byte(req.Content), 0644); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "保存失败: " + err.Error()})
			return
		}

		log.Printf("[config-editor] Config saved (backup: %s)", backupPath)

		// Try hot-reload
		reloadMsg := "配置已保存"
		if ce.onReload != nil {
			if err := ce.onReload(); err != nil {
				reloadMsg = "配置已保存，但重载失败: " + err.Error() + " (需要手动重启)"
			} else {
				reloadMsg = "配置已保存并重载成功"
			}
		} else {
			reloadMsg = "配置已保存 (重启后生效: systemctl restart ai-ops-agent)"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"message": reloadMsg,
			"backup":  backupPath,
		})
	}))
}
