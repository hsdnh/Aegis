package sanitize

import (
	"regexp"
	"strings"

	"github.com/hsdnh/Aegis/pkg/types"
)

// Patterns that match sensitive data in logs/responses.
var sensitivePatterns = []*regexp.Regexp{
	// API keys and tokens
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|passwd|pwd)\s*[=:]\s*['"]?[\w\-\.]{8,}['"]?`),
	regexp.MustCompile(`(?i)bearer\s+[\w\-\.]{20,}`),
	regexp.MustCompile(`(?i)basic\s+[A-Za-z0-9+/=]{20,}`),

	// AWS keys
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)aws[_-]?secret[_-]?access[_-]?key\s*[=:]\s*[\w/+=]{30,}`),

	// Private keys
	regexp.MustCompile(`-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----`),

	// Connection strings with passwords
	regexp.MustCompile(`(?i)(?:mysql|postgres|redis|mongodb)://[^:]+:[^@]+@`),
	regexp.MustCompile(`(?i)password\s*=\s*\S+`),

	// IP addresses (internal) — only in free text, not in structured config fields
	// Note: metric labels with "addr" or "url" keys are excluded via isSensitiveFieldName
	regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`),

	// Email addresses
	regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`),

	// Phone numbers (international)
	regexp.MustCompile(`(?:\+\d{1,3}[-.\s]?)?\(?\d{2,4}\)?[-.\s]?\d{3,4}[-.\s]?\d{4}`),

	// JWT tokens
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+`),

	// Hex tokens (32+ chars)
	regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`),

	// Cookie values
	regexp.MustCompile(`(?i)(?:session|cookie|csrf)[_-]?\w*\s*[=:]\s*\S{16,}`),
}

const redactedPlaceholder = "<REDACTED>"

// SanitizeString removes sensitive patterns from a string.
func SanitizeString(s string) string {
	result := s
	for _, pattern := range sensitivePatterns {
		result = pattern.ReplaceAllString(result, redactedPlaceholder)
	}
	return result
}

// SanitizeLogEntry redacts sensitive data from a log entry.
func SanitizeLogEntry(entry types.LogEntry) types.LogEntry {
	entry.Message = SanitizeString(entry.Message)
	for k, v := range entry.Fields {
		lk := strings.ToLower(k)
		if isSensitiveFieldName(lk) {
			entry.Fields[k] = redactedPlaceholder
		} else {
			entry.Fields[k] = SanitizeString(v)
		}
	}
	return entry
}

// SanitizeCollectResult redacts all sensitive data in a collection result.
func SanitizeCollectResult(result types.CollectResult) types.CollectResult {
	for i, log := range result.Logs {
		result.Logs[i] = SanitizeLogEntry(log)
	}
	for i, err := range result.Errors {
		result.Errors[i] = SanitizeString(err)
	}
	// Metrics labels — preserve operational fields (addr, url, pattern, key)
	// but redact known-sensitive ones (password, token, etc.)
	for i, m := range result.Metrics {
		for k, v := range m.Labels {
			lk := strings.ToLower(k)
			if isSensitiveFieldName(lk) {
				result.Metrics[i].Labels[k] = redactedPlaceholder
			} else if isOperationalField(lk) {
				// Keep as-is — these contain addresses/patterns needed for diagnosis
			} else {
				result.Metrics[i].Labels[k] = SanitizeString(v)
			}
		}
	}
	return result
}

// SanitizeSnapshot redacts all sensitive data before sending to AI.
func SanitizeSnapshot(snapshot types.Snapshot) types.Snapshot {
	for i, r := range snapshot.Results {
		snapshot.Results[i] = SanitizeCollectResult(r)
	}
	return snapshot
}

// isOperationalField checks if a label key contains infrastructure info
// that should NOT be redacted (needed for monitoring/diagnosis).
func isOperationalField(name string) bool {
	ops := []string{"addr", "url", "pattern", "key", "path", "query", "metric", "source", "host", "port"}
	for _, o := range ops {
		if strings.Contains(name, o) {
			return true
		}
	}
	return false
}

// isSensitiveFieldName checks if a field name suggests sensitive content.
func isSensitiveFieldName(name string) bool {
	sensitive := []string{
		"password", "passwd", "pwd", "secret", "token",
		"api_key", "apikey", "auth", "authorization",
		"cookie", "session", "credential", "private_key",
		"access_key", "secret_key", "dsn",
	}
	for _, s := range sensitive {
		if strings.Contains(name, s) {
			return true
		}
	}
	return false
}
