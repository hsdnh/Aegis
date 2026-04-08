// Package changefeed tracks external changes (deploys, config, schema)
// and correlates them with Issues on a timeline.
// "Anomaly started 8 minutes after commit abc1234 was deployed."
package changefeed

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ChangeType categorizes what changed.
type ChangeType string

const (
	ChangeDeploy  ChangeType = "deploy"  // git push / new binary
	ChangeConfig  ChangeType = "config"  // config file modified
	ChangeSchema  ChangeType = "schema"  // DB migration
	ChangeRestart ChangeType = "restart" // process restart
	ChangeManual  ChangeType = "manual"  // user-reported
)

// ChangeEvent records one external change.
type ChangeEvent struct {
	ID          string     `json:"id"`
	Type        ChangeType `json:"type"`
	Timestamp   time.Time  `json:"timestamp"`
	Summary     string     `json:"summary"`     // "commit abc1234: fix mysql DSN"
	Details     string     `json:"details"`      // full diff or description
	Source      string     `json:"source"`       // "git", "file_watcher", "user"
	RelatedFile string     `json:"related_file,omitempty"`
}

// Feed tracks changes and provides timeline queries.
type Feed struct {
	mu       sync.RWMutex
	events   []ChangeEvent
	maxKeep  int
	watchers []WatchConfig
}

// WatchConfig defines what to watch for changes.
type WatchConfig struct {
	Type     ChangeType `yaml:"type"`
	GitRepo  string     `yaml:"git_repo,omitempty"`  // path to git repo
	FilePath string     `yaml:"file_path,omitempty"` // config file to watch
	CheckSQL string     `yaml:"check_sql,omitempty"` // SQL to detect schema changes
}

func NewFeed(maxKeep int) *Feed {
	if maxKeep == 0 {
		maxKeep = 200
	}
	return &Feed{maxKeep: maxKeep}
}

// AddWatcher adds a change source to monitor.
func (f *Feed) AddWatcher(w WatchConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchers = append(f.watchers, w)
}

// Record manually adds a change event.
func (f *Feed) Record(evt ChangeEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if evt.ID == "" {
		evt.ID = fmt.Sprintf("chg-%d", time.Now().UnixNano())
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}
	f.events = append(f.events, evt)
	if len(f.events) > f.maxKeep {
		f.events = f.events[len(f.events)-f.maxKeep:]
	}
}

// Poll checks all watchers for new changes. Called each monitoring cycle.
func (f *Feed) Poll(ctx context.Context) []ChangeEvent {
	var newEvents []ChangeEvent
	for _, w := range f.watchers {
		switch w.Type {
		case ChangeDeploy:
			if evt := f.pollGit(ctx, w.GitRepo); evt != nil {
				newEvents = append(newEvents, *evt)
			}
		case ChangeConfig:
			// File watching handled externally or via stat check
		}
	}
	for _, evt := range newEvents {
		f.Record(evt)
	}
	return newEvents
}

// Since returns all changes after the given time.
func (f *Feed) Since(after time.Time) []ChangeEvent {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var result []ChangeEvent
	for _, e := range f.events {
		if e.Timestamp.After(after) {
			result = append(result, e)
		}
	}
	return result
}

// Recent returns the last N changes.
func (f *Feed) Recent(n int) []ChangeEvent {
	f.mu.RLock()
	defer f.mu.RUnlock()
	total := len(f.events)
	if n > total {
		n = total
	}
	result := make([]ChangeEvent, n)
	for i := 0; i < n; i++ {
		result[i] = f.events[total-1-i]
	}
	return result
}

// NearIssue returns changes within a time window around an issue's first_seen_at.
// "What changed in the 30 minutes before this issue appeared?"
func (f *Feed) NearIssue(issueTime time.Time, windowBefore, windowAfter time.Duration) []ChangeEvent {
	f.mu.RLock()
	defer f.mu.RUnlock()
	start := issueTime.Add(-windowBefore)
	end := issueTime.Add(windowAfter)
	var result []ChangeEvent
	for _, e := range f.events {
		if e.Timestamp.After(start) && e.Timestamp.Before(end) {
			result = append(result, e)
		}
	}
	return result
}

// --- Git polling ---

var lastKnownCommit string

func (f *Feed) pollGit(ctx context.Context, repoPath string) *ChangeEvent {
	if repoPath == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "log", "-1", "--format=%H|%s|%ci")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	line := strings.TrimSpace(string(out))
	parts := strings.SplitN(line, "|", 3)
	if len(parts) < 3 {
		return nil
	}
	commitHash := parts[0][:8]
	commitMsg := parts[1]
	commitTime := parts[2]

	if commitHash == lastKnownCommit {
		return nil // no new commits
	}
	lastKnownCommit = commitHash

	return &ChangeEvent{
		ID:        "git-" + commitHash,
		Type:      ChangeDeploy,
		Timestamp: parseGitTime(commitTime),
		Summary:   fmt.Sprintf("commit %s: %s", commitHash, commitMsg),
		Source:    "git",
	}
}

func parseGitTime(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05 -0700", strings.TrimSpace(s))
	if err != nil {
		return time.Now()
	}
	return t
}
