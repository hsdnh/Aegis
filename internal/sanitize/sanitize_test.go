package sanitize

import (
	"strings"
	"testing"
)

func TestSanitizeAPIKey(t *testing.T) {
	input := `api_key = "sk-abc123456789xyz"`
	result := SanitizeString(input)
	if strings.Contains(result, "sk-abc") {
		t.Error("API key should be redacted")
	}
}

func TestSanitizeBearer(t *testing.T) {
	input := `Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.test`
	result := SanitizeString(input)
	if strings.Contains(result, "eyJ") {
		t.Error("Bearer token should be redacted")
	}
}

func TestSanitizeConnectionString(t *testing.T) {
	input := `mysql://admin:secretpass@10.0.0.5:3306/db`
	result := SanitizeString(input)
	if strings.Contains(result, "secretpass") {
		t.Error("password in connection string should be redacted")
	}
}

func TestSanitizePreservesNormalText(t *testing.T) {
	input := "Queue length is 5000, processing normally"
	result := SanitizeString(input)
	if result != input {
		t.Errorf("normal text should not be modified: got %q", result)
	}
}

func TestIsSensitiveFieldName(t *testing.T) {
	sensitive := []string{"password", "api_key", "secret_token", "authorization"}
	for _, name := range sensitive {
		if !isSensitiveFieldName(name) {
			t.Errorf("%s should be sensitive", name)
		}
	}

	safe := []string{"queue_length", "status_code", "latency"}
	for _, name := range safe {
		if isSensitiveFieldName(name) {
			t.Errorf("%s should NOT be sensitive", name)
		}
	}
}

func TestIsOperationalField(t *testing.T) {
	ops := []string{"addr", "url", "pattern", "key_name"}
	for _, name := range ops {
		if !isOperationalField(name) {
			t.Errorf("%s should be operational", name)
		}
	}
}
