package issue

import (
	"sync"
	"time"
)

// SilenceRule defines a maintenance window that suppresses notifications.
type SilenceRule struct {
	ID        string    `json:"id" yaml:"id"`
	Reason    string    `json:"reason" yaml:"reason"` // "deployment", "maintenance", "testing"
	StartTime time.Time `json:"start_time" yaml:"start_time"`
	EndTime   time.Time `json:"end_time" yaml:"end_time"`

	// Matchers — silence only matching issues. Empty = silence ALL.
	Services  []string `json:"services,omitempty" yaml:"services"`   // match issue source
	Rules     []string `json:"rules,omitempty" yaml:"rules"`         // match rule names
	Severities []string `json:"severities,omitempty" yaml:"severities"` // "warning", "critical"

	CreatedBy string `json:"created_by" yaml:"created_by"`
}

// SilenceManager handles maintenance windows and notification suppression.
type SilenceManager struct {
	mu    sync.RWMutex
	rules []SilenceRule
}

func NewSilenceManager() *SilenceManager {
	return &SilenceManager{}
}

// AddRule creates a new silence window.
func (sm *SilenceManager) AddRule(r SilenceRule) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if r.ID == "" {
		r.ID = time.Now().Format("20060102-150405")
	}
	sm.rules = append(sm.rules, r)
}

// RemoveRule deletes a silence window by ID.
func (sm *SilenceManager) RemoveRule(id string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for i, r := range sm.rules {
		if r.ID == id {
			sm.rules = append(sm.rules[:i], sm.rules[i+1:]...)
			return
		}
	}
}

// IsSilenced checks if an alert should be suppressed right now.
func (sm *SilenceManager) IsSilenced(ruleName, service, severity string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	now := time.Now()

	for _, rule := range sm.rules {
		// Check time window
		if now.Before(rule.StartTime) || now.After(rule.EndTime) {
			continue
		}

		// Empty matchers = silence everything
		if len(rule.Services) == 0 && len(rule.Rules) == 0 && len(rule.Severities) == 0 {
			return true
		}

		// Check matchers (any match = silenced)
		if matchesAny(service, rule.Services) ||
			matchesAny(ruleName, rule.Rules) ||
			matchesAny(severity, rule.Severities) {
			return true
		}
	}
	return false
}

// ActiveRules returns currently active silence windows.
func (sm *SilenceManager) ActiveRules() []SilenceRule {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	now := time.Now()
	var active []SilenceRule
	for _, r := range sm.rules {
		if now.After(r.StartTime) && now.Before(r.EndTime) {
			active = append(active, r)
		}
	}
	return active
}

// AllRules returns all silence rules (including expired).
func (sm *SilenceManager) AllRules() []SilenceRule {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.rules
}

// CleanExpired removes rules that ended more than 24h ago.
func (sm *SilenceManager) CleanExpired() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	var kept []SilenceRule
	for _, r := range sm.rules {
		if r.EndTime.After(cutoff) {
			kept = append(kept, r)
		}
	}
	sm.rules = kept
}

func matchesAny(value string, patterns []string) bool {
	for _, p := range patterns {
		if p == value {
			return true
		}
	}
	return false
}
