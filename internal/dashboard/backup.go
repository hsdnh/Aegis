// Backup/restore source code before instrument operations.
package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BackupManager handles source code backup and restore.
type BackupManager struct {
	backupDir string // e.g. /opt/ai-ops-agent/backups
}

type BackupInfo struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	SourceDir string    `json:"source_dir"`
	SizeMB    float64   `json:"size_mb"`
	CreatedAt time.Time `json:"created_at"`
}

func NewBackupManager(backupDir string) *BackupManager {
	if backupDir == "" {
		backupDir = "/opt/ai-ops-agent/backups"
	}
	os.MkdirAll(backupDir, 0755)
	return &BackupManager{backupDir: backupDir}
}

// Backup creates a tar.gz backup of the source directory.
func (bm *BackupManager) Backup(sourceDir string) (*BackupInfo, error) {
	if sourceDir == "" {
		return nil, fmt.Errorf("source directory not configured")
	}

	id := time.Now().Format("20060102-150405")
	name := fmt.Sprintf("backup-%s-%s.tar.gz", filepath.Base(sourceDir), id)
	backupPath := filepath.Join(bm.backupDir, name)

	log.Printf("[backup] Creating backup: %s → %s", sourceDir, backupPath)

	cmd := exec.Command("tar", "czf", backupPath, "-C", filepath.Dir(sourceDir), filepath.Base(sourceDir))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tar failed: %v — %s", err, string(out))
	}

	info, _ := os.Stat(backupPath)
	sizeMB := float64(0)
	if info != nil {
		sizeMB = float64(info.Size()) / 1024 / 1024
	}

	log.Printf("[backup] Backup created: %s (%.1f MB)", backupPath, sizeMB)

	return &BackupInfo{
		ID:        id,
		Path:      backupPath,
		SourceDir: sourceDir,
		SizeMB:    sizeMB,
		CreatedAt: time.Now(),
	}, nil
}

// Restore restores from a backup tar.gz.
func (bm *BackupManager) Restore(backupPath, targetDir string) error {
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("backup not found: %s", backupPath)
	}

	log.Printf("[backup] Restoring: %s → %s", backupPath, targetDir)

	// Remove current and extract
	parentDir := filepath.Dir(targetDir)
	baseName := filepath.Base(targetDir)

	// Rename current as safety
	safetyPath := targetDir + ".pre-restore"
	os.Rename(targetDir, safetyPath)

	cmd := exec.Command("tar", "xzf", backupPath, "-C", parentDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Restore failed — put back the safety copy
		os.Rename(safetyPath, targetDir)
		return fmt.Errorf("restore failed: %v — %s", err, string(out))
	}

	// Verify extraction
	if _, err := os.Stat(filepath.Join(parentDir, baseName)); err != nil {
		os.Rename(safetyPath, targetDir)
		return fmt.Errorf("extraction didn't produce expected directory")
	}

	// Clean up safety copy
	os.RemoveAll(safetyPath)
	log.Printf("[backup] Restored successfully")
	return nil
}

// List returns all available backups.
func (bm *BackupManager) List() []BackupInfo {
	var backups []BackupInfo
	entries, err := os.ReadDir(bm.backupDir)
	if err != nil {
		return backups
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, _ := e.Info()
		sizeMB := float64(0)
		if info != nil {
			sizeMB = float64(info.Size()) / 1024 / 1024
		}

		backups = append(backups, BackupInfo{
			ID:        strings.TrimSuffix(e.Name(), ".tar.gz"),
			Path:      filepath.Join(bm.backupDir, e.Name()),
			SizeMB:    sizeMB,
			CreatedAt: info.ModTime(),
		})
	}
	return backups
}

// RegisterRoutes adds backup/restore API endpoints.
func (bm *BackupManager) RegisterRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc, getSourcePath func() string) {
	// List backups
	mux.HandleFunc("/api/backup/list", wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bm.List())
	}))

	// Create backup
	mux.HandleFunc("/api/backup/create", wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST", 405)
			return
		}
		srcPath := getSourcePath()
		if srcPath == "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "source_path not configured"})
			return
		}
		info, err := bm.Backup(srcPath)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	}))

	// Restore from backup
	mux.HandleFunc("/api/backup/restore", wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST", 405)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			BackupPath string `json:"backup_path"`
			TargetDir  string `json:"target_dir"`
		}
		json.Unmarshal(body, &req)

		if req.BackupPath == "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "backup_path required"})
			return
		}
		if req.TargetDir == "" {
			req.TargetDir = getSourcePath()
		}

		if err := bm.Restore(req.BackupPath, req.TargetDir); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "源码已恢复"})
	}))
}
