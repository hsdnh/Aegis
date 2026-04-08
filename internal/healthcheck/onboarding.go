// Package healthcheck validates agent configuration and connectivity on startup.
// "Is Redis reachable? Can we read logs? Is SQLite working?"
// This runs before the first monitoring cycle to catch setup errors early.
package healthcheck

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/redis/go-redis/v9"
)

// CheckResult records one preflight check.
type CheckResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // "pass", "fail", "skip", "warn"
	Message string `json:"message"`
	Duration int64  `json:"duration_ms"`
}

// PreflightReport is the full startup health check report.
type PreflightReport struct {
	Checks    []CheckResult `json:"checks"`
	PassCount int           `json:"pass_count"`
	FailCount int           `json:"fail_count"`
	WarnCount int           `json:"warn_count"`
	AllPassed bool          `json:"all_passed"`
	Timestamp time.Time     `json:"timestamp"`
}

// RunPreflight checks everything needed before the agent starts monitoring.
func RunPreflight(ctx context.Context, cfg PreflightConfig) *PreflightReport {
	report := &PreflightReport{Timestamp: time.Now()}

	// Platform check
	report.add(checkPlatform())

	// Redis connectivity
	for _, addr := range cfg.RedisAddrs {
		report.add(checkRedis(ctx, addr, cfg.RedisPassword))
	}

	// MySQL connectivity
	for _, dsn := range cfg.MySQLDSNs {
		report.add(checkMySQL(ctx, dsn))
	}

	// HTTP endpoints
	for _, url := range cfg.HTTPURLs {
		report.add(checkHTTP(ctx, url))
	}

	// Log sources
	for _, src := range cfg.LogSources {
		report.add(checkLogSource(src))
	}

	// SQLite (if storage enabled)
	if cfg.SQLitePath != "" {
		report.add(checkSQLite(cfg.SQLitePath))
	}

	// Disk space
	report.add(checkDiskSpace())

	// DNS / network
	report.add(checkNetwork(ctx))

	// Summary
	for _, c := range report.Checks {
		switch c.Status {
		case "pass":
			report.PassCount++
		case "fail":
			report.FailCount++
		case "warn":
			report.WarnCount++
		}
	}
	report.AllPassed = report.FailCount == 0

	return report
}

// PreflightConfig specifies what to check.
type PreflightConfig struct {
	RedisAddrs    []string
	RedisPassword string
	MySQLDSNs     []string
	HTTPURLs      []string
	LogSources    []LogSourceCheck
	SQLitePath    string
}

type LogSourceCheck struct {
	Source   string // "journalctl", "file", "docker"
	Unit    string
	Path    string
	Container string
}

func (r *PreflightReport) add(c CheckResult) {
	r.Checks = append(r.Checks, c)
}

// --- Individual checks ---

func checkPlatform() CheckResult {
	return CheckResult{
		Name:    "Platform",
		Status:  "pass",
		Message: fmt.Sprintf("%s/%s, Go %s", runtime.GOOS, runtime.GOARCH, runtime.Version()),
	}
}

func checkRedis(ctx context.Context, addr, password string) CheckResult {
	start := time.Now()
	name := fmt.Sprintf("Redis (%s)", addr)

	client := redis.NewClient(&redis.Options{Addr: addr, Password: password})
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := client.Ping(ctx).Err()
	dur := time.Since(start).Milliseconds()

	if err != nil {
		return CheckResult{Name: name, Status: "fail", Message: err.Error(), Duration: dur}
	}

	info, _ := client.Info(ctx, "server").Result()
	version := "unknown"
	for _, line := range splitLines(info) {
		if len(line) > 14 && line[:14] == "redis_version:" {
			version = line[14:]
			break
		}
	}
	return CheckResult{Name: name, Status: "pass", Message: fmt.Sprintf("OK (v%s, %dms)", version, dur), Duration: dur}
}

func checkMySQL(ctx context.Context, dsn string) CheckResult {
	start := time.Now()
	name := "MySQL"

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return CheckResult{Name: name, Status: "fail", Message: err.Error()}
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err = db.PingContext(ctx)
	dur := time.Since(start).Milliseconds()

	if err != nil {
		return CheckResult{Name: name, Status: "fail", Message: err.Error(), Duration: dur}
	}
	return CheckResult{Name: name, Status: "pass", Message: fmt.Sprintf("OK (%dms)", dur), Duration: dur}
}

func checkHTTP(ctx context.Context, url string) CheckResult {
	start := time.Now()
	name := fmt.Sprintf("HTTP (%s)", url)

	conn, err := net.DialTimeout("tcp", extractHost(url), 5*time.Second)
	dur := time.Since(start).Milliseconds()

	if err != nil {
		return CheckResult{Name: name, Status: "fail", Message: err.Error(), Duration: dur}
	}
	conn.Close()
	return CheckResult{Name: name, Status: "pass", Message: fmt.Sprintf("TCP OK (%dms)", dur), Duration: dur}
}

func checkLogSource(src LogSourceCheck) CheckResult {
	name := fmt.Sprintf("Log (%s)", src.Source)

	switch src.Source {
	case "journalctl":
		if runtime.GOOS == "windows" {
			return CheckResult{Name: name, Status: "warn", Message: "journalctl not available on Windows — use 'file' source"}
		}
		_, err := exec.LookPath("journalctl")
		if err != nil {
			return CheckResult{Name: name, Status: "fail", Message: "journalctl not found in PATH"}
		}
		return CheckResult{Name: name, Status: "pass", Message: "OK"}

	case "file":
		if src.Path == "" {
			return CheckResult{Name: name, Status: "warn", Message: "no file_path configured"}
		}
		if _, err := os.Stat(src.Path); err != nil {
			return CheckResult{Name: name, Status: "warn", Message: fmt.Sprintf("file not found: %s (will be created at runtime?)", src.Path)}
		}
		return CheckResult{Name: name, Status: "pass", Message: fmt.Sprintf("OK (%s)", src.Path)}

	case "docker":
		_, err := exec.LookPath("docker")
		if err != nil {
			return CheckResult{Name: name, Status: "fail", Message: "docker not found in PATH"}
		}
		return CheckResult{Name: name, Status: "pass", Message: "OK"}

	default:
		return CheckResult{Name: name, Status: "warn", Message: "unknown source type: " + src.Source}
	}
}

func checkSQLite(path string) CheckResult {
	name := "SQLite"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return CheckResult{Name: name, Status: "fail", Message: err.Error()}
	}
	defer db.Close()

	_, err = db.Exec("CREATE TABLE IF NOT EXISTS _preflight_test (id INTEGER); DROP TABLE IF EXISTS _preflight_test;")
	if err != nil {
		return CheckResult{Name: name, Status: "fail", Message: err.Error()}
	}
	return CheckResult{Name: name, Status: "pass", Message: fmt.Sprintf("OK (%s)", path)}
}

func checkDiskSpace() CheckResult {
	name := "Disk Space"
	// Simple check: can we write?
	f, err := os.CreateTemp("", "aiops-preflight-*")
	if err != nil {
		return CheckResult{Name: name, Status: "fail", Message: "cannot write to temp dir: " + err.Error()}
	}
	fname := f.Name()
	f.Close()
	os.Remove(fname)
	return CheckResult{Name: name, Status: "pass", Message: "OK (writable)"}
}

func checkNetwork(ctx context.Context) CheckResult {
	name := "Network"
	targets := []string{"114.114.114.114:53", "8.8.8.8:53", "1.1.1.1:53"}
	for _, t := range targets {
		conn, err := net.DialTimeout("tcp", t, 3*time.Second)
		if err == nil {
			conn.Close()
			return CheckResult{Name: name, Status: "pass", Message: fmt.Sprintf("OK (reached %s)", t)}
		}
	}
	return CheckResult{Name: name, Status: "warn", Message: "cannot reach external DNS — agent may misdetect network partitions"}
}

// --- helpers ---

func extractHost(url string) string {
	// "http://localhost:8080/path" → "localhost:8080"
	s := url
	if i := findStr(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := findStr(s, "/"); i >= 0 {
		s = s[:i]
	}
	if !containsStr(s, ":") {
		s += ":80"
	}
	return s
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func findStr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func containsStr(s, sub string) bool {
	return findStr(s, sub) >= 0
}

// FormatReport returns a pretty-printed preflight report for console output.
func (r *PreflightReport) FormatReport() string {
	var sb []byte
	sb = append(sb, "\n=== Preflight Health Check ===\n\n"...)
	for _, c := range r.Checks {
		icon := "✅"
		switch c.Status {
		case "fail":
			icon = "❌"
		case "warn":
			icon = "⚠️"
		case "skip":
			icon = "⏭️"
		}
		line := fmt.Sprintf("  %s %-20s %s\n", icon, c.Name, c.Message)
		sb = append(sb, line...)
	}
	sb = append(sb, fmt.Sprintf("\n  Result: %d passed, %d failed, %d warnings\n", r.PassCount, r.FailCount, r.WarnCount)...)
	if r.AllPassed {
		sb = append(sb, "  ✅ All checks passed — ready to start monitoring\n"...)
	} else {
		sb = append(sb, "  ❌ Some checks failed — review configuration\n"...)
	}
	return string(sb)
}
