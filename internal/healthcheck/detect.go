// Auto-detect project source code paths on the server.
// Scans common locations to find Go/Python/Node projects.
package healthcheck

import (
	"os"
	"path/filepath"
	"strings"
)

// DetectedProject describes a found project on the server.
type DetectedProject struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Language string `json:"language"` // "go", "python", "node", "unknown"
	HasGit   bool   `json:"has_git"`
}

// DetectProjects scans common directories for source code projects.
func DetectProjects() []DetectedProject {
	var projects []DetectedProject

	// Common locations to search
	searchDirs := []string{
		"/opt",
		"/home",
		"/root",
		"/var/www",
		"/srv",
		"/app",
		"/data",
	}

	// Also check env hints
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		searchDirs = append(searchDirs, filepath.Join(gopath, "src"))
	}
	if home := os.Getenv("HOME"); home != "" {
		searchDirs = append(searchDirs, home)
	}

	seen := make(map[string]bool)
	for _, dir := range searchDirs {
		scanDir(dir, 2, &projects, seen) // max 2 levels deep
	}

	return projects
}

func scanDir(dir string, maxDepth int, projects *[]DetectedProject, seen map[string]bool) {
	if maxDepth < 0 {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Skip hidden dirs and common non-project dirs
		if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" ||
			name == "venv" || name == "__pycache__" || name == "lost+found" {
			continue
		}

		fullPath := filepath.Join(dir, name)
		if seen[fullPath] {
			continue
		}

		// Check if this directory looks like a project
		lang := detectLanguage(fullPath)
		if lang != "" {
			seen[fullPath] = true
			hasGit := fileExists(filepath.Join(fullPath, ".git"))
			*projects = append(*projects, DetectedProject{
				Path:     fullPath,
				Name:     name,
				Language: lang,
				HasGit:   hasGit,
			})
		} else {
			// Recurse deeper
			scanDir(fullPath, maxDepth-1, projects, seen)
		}
	}
}

func detectLanguage(dir string) string {
	// Go project
	if fileExists(filepath.Join(dir, "go.mod")) {
		return "go"
	}
	// Python project
	if fileExists(filepath.Join(dir, "requirements.txt")) || fileExists(filepath.Join(dir, "setup.py")) ||
		fileExists(filepath.Join(dir, "pyproject.toml")) {
		return "python"
	}
	// Node.js project
	if fileExists(filepath.Join(dir, "package.json")) {
		return "node"
	}
	// Rust
	if fileExists(filepath.Join(dir, "Cargo.toml")) {
		return "rust"
	}
	// Java
	if fileExists(filepath.Join(dir, "pom.xml")) || fileExists(filepath.Join(dir, "build.gradle")) {
		return "java"
	}
	// Docker project (has code inside)
	if fileExists(filepath.Join(dir, "Dockerfile")) && fileExists(filepath.Join(dir, "main.go")) {
		return "go"
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
