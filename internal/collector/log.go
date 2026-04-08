package collector

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/hsdnh/Aegis/pkg/types"
)

type LogCollectorConfig struct {
	Source        string   `yaml:"source"`         // "journalctl", "file", "docker"
	Unit          string   `yaml:"unit"`           // for journalctl
	FilePath      string   `yaml:"file_path"`      // for file source
	Container     string   `yaml:"container"`      // for docker
	ErrorPatterns []string `yaml:"error_patterns"` // patterns to match
	Minutes       int      `yaml:"minutes"`        // look back N minutes
}

type LogCollector struct {
	cfg LogCollectorConfig
}

func NewLogCollector(cfg LogCollectorConfig) *LogCollector {
	if cfg.Minutes == 0 {
		cfg.Minutes = 30
	}
	return &LogCollector{cfg: cfg}
}

func (l *LogCollector) Name() string { return "log" }

func (l *LogCollector) Collect(ctx context.Context) (*types.CollectResult, error) {
	now := time.Now()
	result := &types.CollectResult{
		CollectorName: l.Name(),
		CollectedAt:   now,
	}

	lines, err := l.fetchLogs(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch logs: %w", err)
	}

	var errorCount, warnCount, totalCount int
	for _, line := range lines {
		totalCount++
		lower := strings.ToLower(line)

		matched := false
		for _, pattern := range l.cfg.ErrorPatterns {
			if strings.Contains(lower, strings.ToLower(pattern)) {
				matched = true
				break
			}
		}

		level := "INFO"
		if matched || strings.Contains(lower, "error") || strings.Contains(lower, "panic") || strings.Contains(lower, "fatal") {
			level = "ERROR"
			errorCount++
		} else if strings.Contains(lower, "warn") {
			level = "WARNING"
			warnCount++
		}

		if level != "INFO" {
			result.Logs = append(result.Logs, types.LogEntry{
				Timestamp: now,
				Level:     level,
				Message:   truncate(line, 500),
				Source:    fmt.Sprintf("log:%s", l.cfg.Source),
			})
		}
	}

	result.Metrics = append(result.Metrics, types.Metric{
		Name: "log.lines.total", Value: float64(totalCount), Timestamp: now, Source: "log",
	})
	result.Metrics = append(result.Metrics, types.Metric{
		Name: "log.lines.errors", Value: float64(errorCount), Timestamp: now, Source: "log",
	})
	result.Metrics = append(result.Metrics, types.Metric{
		Name: "log.lines.warnings", Value: float64(warnCount), Timestamp: now, Source: "log",
	})

	return result, nil
}

func (l *LogCollector) fetchLogs(ctx context.Context) ([]string, error) {
	var cmd *exec.Cmd

	switch l.cfg.Source {
	case "journalctl":
		cmd = exec.CommandContext(ctx, "journalctl",
			"-u", l.cfg.Unit,
			"--since", fmt.Sprintf("%d min ago", l.cfg.Minutes),
			"--no-pager",
			"-o", "short-iso",
		)
	case "docker":
		cmd = exec.CommandContext(ctx, "docker", "logs",
			"--since", fmt.Sprintf("%dm", l.cfg.Minutes),
			l.cfg.Container,
		)
	case "file":
		// Use tail on Linux/Mac, PowerShell on Windows
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "powershell", "-Command",
				fmt.Sprintf("Get-Content -Tail 1000 '%s'", l.cfg.FilePath))
		} else {
			cmd = exec.CommandContext(ctx, "tail", "-n", "1000", l.cfg.FilePath)
		}
	default:
		return nil, fmt.Errorf("unknown log source: %s", l.cfg.Source)
	}

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("command failed: %w", err)
	}

	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
