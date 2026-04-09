// Auto-configuration: scan project files to extract Redis/MySQL/HTTP connection info
// and generate a complete monitoring config without manual input.
package healthcheck

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ExtractedConfig holds connection info extracted from project files.
type ExtractedConfig struct {
	ProjectName string          `json:"project_name"`
	ProjectPath string          `json:"project_path"`
	Redis       []RedisInfo     `json:"redis,omitempty"`
	MySQL       []MySQLInfo     `json:"mysql,omitempty"`
	HTTP        []HTTPInfo      `json:"http,omitempty"`
	LogPaths    []string        `json:"log_paths,omitempty"`
	LogSources  []LogSource     `json:"log_sources,omitempty"`
	EnvFiles    []string        `json:"env_files"`
}

type RedisInfo struct {
	Addr     string `json:"addr"`
	Password string `json:"password"`
	DB       string `json:"db"`
	Prefix   string `json:"prefix"`
}

type MySQLInfo struct {
	DSN  string `json:"dsn"`
	Host string `json:"host"`
	Port string `json:"port"`
	User string `json:"user"`
	Pass string `json:"pass"`
	DB   string `json:"db"`
}

type HTTPInfo struct {
	URL  string `json:"url"`
	Port string `json:"port"`
	User string `json:"user"`
	Pass string `json:"pass"`
}

// AutoExtractConfig scans a project directory for connection info.
// Reads: .env files, config.yaml, config.json, docker-compose.yml, etc.
func AutoExtractConfig(projectPath string) *ExtractedConfig {
	cfg := &ExtractedConfig{
		ProjectName: filepath.Base(projectPath),
		ProjectPath: projectPath,
	}

	// Find all config-like files
	configFiles := findConfigFiles(projectPath)

	// Parse each file for connection info
	for _, f := range configFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		cfg.EnvFiles = append(cfg.EnvFiles, f)
		content := string(data)

		extractRedis(content, cfg)
		extractMySQL(content, cfg)
		extractHTTP(content, cfg)
	}

	// Find log directories
	cfg.LogSources = findLogSources(projectPath)
	cfg.LogPaths = findLogPaths(projectPath)

	return cfg
}

func findConfigFiles(root string) []string {
	var files []string
	patterns := []string{
		"*.env", ".env", "*.yaml", "*.yml", "*.toml",
		"*.conf", "*.ini", "*.json", "docker-compose*.yml",
	}

	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() {
				name := info.Name()
				if name == ".git" || name == "node_modules" || name == "vendor" || name == "__pycache__" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		for _, p := range patterns {
			matched, _ := filepath.Match(p, info.Name())
			if matched {
				files = append(files, path)
				break
			}
		}
		return nil
	})
	return files
}

// --- Redis extraction ---

var redisPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:REDIS_ADDR|REDIS_HOST|REDIS_URL)\s*[=:]\s*["']?([^"'\s\n]+)`),
	regexp.MustCompile(`(?i)(?:REDIS_PASS|REDIS_PASSWORD)\s*[=:]\s*["']?([^"'\s\n]+)`),
	regexp.MustCompile(`(?i)(?:REDIS_DB)\s*[=:]\s*["']?(\d+)`),
	regexp.MustCompile(`(?i)(?:REDIS_PREFIX)\s*[=:]\s*["']?([^"'\s\n]+)`),
	regexp.MustCompile(`redis://(?::([^@]+)@)?([^/:]+):?(\d*)/?\d*`),
}

func extractRedis(content string, cfg *ExtractedConfig) {
	info := RedisInfo{DB: "0"}

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		upper := strings.ToUpper(line)

		if strings.Contains(upper, "REDIS_ADDR") || strings.Contains(upper, "REDIS_HOST") {
			if v := extractValue(line); v != "" {
				addr := v
				// Check if there's a separate port
				if !strings.Contains(addr, ":") {
					port := findValueInContent(content, "REDIS_PORT")
					if port != "" {
						addr = addr + ":" + port
					} else {
						addr = addr + ":6379"
					}
				}
				info.Addr = addr
			}
		}
		if strings.Contains(upper, "REDIS_PASS") {
			if v := extractValue(line); v != "" {
				info.Password = v
			}
		}
		if strings.Contains(upper, "REDIS_DB") && !strings.Contains(upper, "REDIS_DB_") {
			if v := extractValue(line); v != "" {
				info.DB = v
			}
		}
		if strings.Contains(upper, "REDIS_PREFIX") {
			if v := extractValue(line); v != "" {
				info.Prefix = v
			}
		}
	}

	// Also try redis:// URL format
	for _, m := range redisPatterns[4].FindAllStringSubmatch(content, -1) {
		if len(m) >= 3 && info.Addr == "" {
			port := "6379"
			if len(m) >= 4 && m[3] != "" {
				port = m[3]
			}
			info.Addr = m[2] + ":" + port
			if m[1] != "" {
				info.Password = m[1]
			}
		}
	}

	if info.Addr != "" {
		// Deduplicate
		for _, existing := range cfg.Redis {
			if existing.Addr == info.Addr {
				return
			}
		}
		cfg.Redis = append(cfg.Redis, info)
	}
}

// --- MySQL extraction ---

func extractMySQL(content string, cfg *ExtractedConfig) {
	info := MySQLInfo{Port: "3306"}

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		upper := strings.ToUpper(line)

		if strings.Contains(upper, "MYSQL_HOST") || strings.Contains(upper, "DB_HOST") {
			if v := extractValue(line); v != "" {
				info.Host = v
			}
		}
		if strings.Contains(upper, "MYSQL_PORT") || strings.Contains(upper, "DB_PORT") {
			if v := extractValue(line); v != "" {
				info.Port = v
			}
		}
		if (strings.Contains(upper, "MYSQL_USER") || strings.Contains(upper, "DB_USER")) && !strings.Contains(upper, "USERNAME") {
			if v := extractValue(line); v != "" {
				info.User = v
			}
		}
		if (strings.Contains(upper, "MYSQL_PASS") || strings.Contains(upper, "DB_PASS")) && !strings.Contains(upper, "PASSWORD_") {
			if v := extractValue(line); v != "" {
				info.Pass = v
			}
		}
		if (strings.Contains(upper, "MYSQL_DB") || strings.Contains(upper, "DB_NAME") || strings.Contains(upper, "DATABASE_NAME")) &&
			!strings.Contains(upper, "MYSQL_DB_") {
			if v := extractValue(line); v != "" {
				info.DB = v
			}
		}

		// DSN format: mysql://user:pass@host:port/db or user:pass@tcp(host:port)/db
		if strings.Contains(line, "mysql://") || strings.Contains(line, "@tcp(") {
			if v := extractValue(line); v != "" {
				info.DSN = v
			}
		}
	}

	if info.Host != "" && info.User != "" {
		info.DSN = fmt.Sprintf("%s:%s@tcp(%s:%s)/%s", info.User, info.Pass, info.Host, info.Port, info.DB)
		for _, existing := range cfg.MySQL {
			if existing.DSN == info.DSN {
				return
			}
		}
		cfg.MySQL = append(cfg.MySQL, info)
	} else if info.DSN != "" {
		cfg.MySQL = append(cfg.MySQL, info)
	}
}

// --- HTTP extraction ---

func extractHTTP(content string, cfg *ExtractedConfig) {
	info := HTTPInfo{}

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		upper := strings.ToUpper(line)

		if strings.Contains(upper, "LISTEN") || strings.Contains(upper, "HTTP_PORT") || strings.Contains(upper, "API_PORT") {
			if v := extractValue(line); v != "" {
				info.Port = strings.TrimPrefix(v, ":")
				if !strings.Contains(info.Port, ":") {
					info.URL = "http://127.0.0.1:" + info.Port
				} else {
					info.URL = "http://127.0.0.1" + v
				}
			}
		}
		if strings.Contains(upper, "ADMIN_USER") {
			if v := extractValue(line); v != "" {
				info.User = v
			}
		}
		if strings.Contains(upper, "ADMIN_PASS") {
			if v := extractValue(line); v != "" {
				info.Pass = v
			}
		}
	}

	if info.URL != "" {
		for _, existing := range cfg.HTTP {
			if existing.URL == info.URL {
				return
			}
		}
		cfg.HTTP = append(cfg.HTTP, info)
	}
}

// LogSource describes how to collect logs for this project.
type LogSource struct {
	Type      string `json:"type"` // "journalctl", "file", "docker"
	Unit      string `json:"unit,omitempty"`
	FilePath  string `json:"file_path,omitempty"`
	Container string `json:"container,omitempty"`
}

// --- Log source detection ---

func findLogSources(root string) []LogSource {
	var sources []LogSource

	// 1. Check systemd services first (most reliable on Linux)
	projectName := strings.ToLower(filepath.Base(root))
	systemdUnits := findSystemdUnits(projectName)
	for _, unit := range systemdUnits {
		sources = append(sources, LogSource{Type: "journalctl", Unit: unit})
	}

	// 2. Check for actual log files in project dirs
	candidates := []string{
		filepath.Join(root, "logs"),
		filepath.Join(root, "log"),
		filepath.Join(root, "master", "logs"),
		filepath.Join(root, "worker", "logs"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			entries, _ := os.ReadDir(c)
			for _, e := range entries {
				if !e.IsDir() && (strings.HasSuffix(e.Name(), ".log") || strings.HasSuffix(e.Name(), ".out")) {
					sources = append(sources, LogSource{
						Type:     "file",
						FilePath: filepath.Join(c, e.Name()),
					})
				}
			}
		}
	}

	// 3. Check for docker containers
	if dockerContainers := findDockerContainers(projectName); len(dockerContainers) > 0 {
		for _, c := range dockerContainers {
			sources = append(sources, LogSource{Type: "docker", Container: c})
		}
	}

	return sources
}

func findSystemdUnits(projectName string) []string {
	var units []string
	// Try common service name patterns
	candidates := []string{
		projectName,
		projectName + "-master",
		projectName + "-worker",
		strings.ReplaceAll(projectName, "-", "_"),
	}

	for _, name := range candidates {
		// Check if unit exists and is loaded
		out, err := execCmd("systemctl", "is-active", name+".service")
		if err == nil && (strings.TrimSpace(out) == "active" || strings.TrimSpace(out) == "inactive") {
			units = append(units, name)
		}
	}
	return units
}

func findDockerContainers(projectName string) []string {
	out, err := execCmd("docker", "ps", "--format", "{{.Names}}")
	if err != nil {
		return nil
	}
	var matched []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if name != "" && strings.Contains(strings.ToLower(name), projectName) {
			matched = append(matched, name)
		}
	}
	return matched
}

func execCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// Legacy helper for backward compat
func findLogPaths(root string) []string {
	sources := findLogSources(root)
	var paths []string
	for _, s := range sources {
		if s.FilePath != "" {
			paths = append(paths, s.FilePath)
		}
	}
	return paths
}

// --- Helpers ---

func extractValue(line string) string {
	// Handle: KEY=VALUE, KEY = VALUE, KEY: VALUE
	for _, sep := range []string{"=", ":"} {
		idx := strings.Index(line, sep)
		if idx > 0 {
			val := strings.TrimSpace(line[idx+1:])
			val = strings.Trim(val, `"'`)
			if val != "" && val != "null" && val != "nil" {
				return val
			}
		}
	}
	return ""
}

func findValueInContent(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(strings.ToUpper(line), strings.ToUpper(key)) {
			return extractValue(line)
		}
	}
	return ""
}

// ToYAML generates a config.yaml snippet from extracted info.
func (ec *ExtractedConfig) ToYAML() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("project: %s\n", ec.ProjectName))
	sb.WriteString(fmt.Sprintf("source_path: \"%s\"\n", ec.ProjectPath))
	sb.WriteString("schedule: \"*/5 * * * *\"\n\n")

	sb.WriteString("collectors:\n")

	if len(ec.Redis) > 0 {
		sb.WriteString("  redis:\n")
		for _, r := range ec.Redis {
			sb.WriteString(fmt.Sprintf("    - addr: \"%s\"\n", r.Addr))
			sb.WriteString(fmt.Sprintf("      password: \"%s\"\n", r.Password))
			sb.WriteString(fmt.Sprintf("      db: %s\n", r.DB))
			if r.Prefix != "" {
				sb.WriteString(fmt.Sprintf("      checks:\n"))
				sb.WriteString(fmt.Sprintf("        - key_pattern: \"%s:queue:*:pending\"\n", r.Prefix))
				sb.WriteString(fmt.Sprintf("          threshold: 1000\n"))
				sb.WriteString(fmt.Sprintf("          alert: \"队列堆积\"\n"))
			}
		}
	}

	if len(ec.MySQL) > 0 {
		sb.WriteString("  mysql:\n")
		for _, m := range ec.MySQL {
			sb.WriteString(fmt.Sprintf("    - dsn: \"%s\"\n", m.DSN))
		}
	}

	if len(ec.HTTP) > 0 {
		sb.WriteString("  http:\n")
		for _, h := range ec.HTTP {
			sb.WriteString(fmt.Sprintf("    - url: \"%s\"\n", h.URL))
			if h.User != "" {
				sb.WriteString(fmt.Sprintf("      auth: \"basic:%s:%s\"\n", h.User, h.Pass))
			}
			sb.WriteString("      timeout: 10\n")
		}
	}

	if len(ec.LogSources) > 0 {
		sb.WriteString("  log:\n")
		for _, ls := range ec.LogSources {
			switch ls.Type {
			case "journalctl":
				sb.WriteString(fmt.Sprintf("    - source: journalctl\n"))
				sb.WriteString(fmt.Sprintf("      unit: %s\n", ls.Unit))
			case "file":
				sb.WriteString(fmt.Sprintf("    - source: file\n"))
				sb.WriteString(fmt.Sprintf("      file_path: \"%s\"\n", ls.FilePath))
			case "docker":
				sb.WriteString(fmt.Sprintf("    - source: docker\n"))
				sb.WriteString(fmt.Sprintf("      container: %s\n", ls.Container))
			}
			sb.WriteString("      error_patterns:\n        - error\n        - panic\n        - fatal\n        - timeout\n      minutes: 30\n")
		}
	} else if len(ec.LogPaths) > 0 {
		sb.WriteString("  log:\n")
		for _, lp := range ec.LogPaths {
			sb.WriteString(fmt.Sprintf("    - source: file\n"))
			sb.WriteString(fmt.Sprintf("      file_path: \"%s\"\n", lp))
			sb.WriteString("      error_patterns:\n        - error\n        - panic\n        - fatal\n        - timeout\n      minutes: 30\n")
		}
	}

	return sb.String()
}
