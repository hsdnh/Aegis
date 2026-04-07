package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

type HTTPCheck struct {
	JSONPath  string  `yaml:"json_path"`
	Name      string  `yaml:"name"`
	Threshold float64 `yaml:"threshold"`
	Alert     string  `yaml:"alert"`
}

type HTTPCollectorConfig struct {
	URL     string      `yaml:"url"`
	Auth    string      `yaml:"auth"` // "basic:user:pass" or "bearer:token"
	Timeout int         `yaml:"timeout"`
	Checks  []HTTPCheck `yaml:"checks"`
}

type HTTPCollector struct {
	cfg    HTTPCollectorConfig
	client *http.Client
}

func NewHTTPCollector(cfg HTTPCollectorConfig) *HTTPCollector {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &HTTPCollector{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

func (h *HTTPCollector) Name() string { return "http" }

func (h *HTTPCollector) Collect(ctx context.Context) (*types.CollectResult, error) {
	now := time.Now()
	result := &types.CollectResult{
		CollectorName: h.Name(),
		CollectedAt:   now,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", h.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("http request create: %w", err)
	}

	// Apply auth
	if h.cfg.Auth != "" {
		parts := strings.SplitN(h.cfg.Auth, ":", 3)
		switch parts[0] {
		case "basic":
			if len(parts) == 3 {
				encoded := base64.StdEncoding.EncodeToString([]byte(parts[1] + ":" + parts[2]))
				req.Header.Set("Authorization", "Basic "+encoded)
			}
		case "bearer":
			if len(parts) >= 2 {
				req.Header.Set("Authorization", "Bearer "+parts[1])
			}
		}
	}

	start := time.Now()
	resp, err := h.client.Do(req)
	latency := time.Since(start)

	result.Metrics = append(result.Metrics, types.Metric{
		Name:      "http.response.latency_ms",
		Value:     float64(latency.Milliseconds()),
		Labels:    map[string]string{"url": h.cfg.URL},
		Timestamp: now,
		Source:    "http",
	})

	if err != nil {
		result.Metrics = append(result.Metrics, types.Metric{
			Name: "http.response.alive", Value: 0,
			Labels: map[string]string{"url": h.cfg.URL}, Timestamp: now, Source: "http",
		})
		result.Errors = append(result.Errors, fmt.Sprintf("http request: %v", err))
		return result, nil // return partial result with latency + alive=0, not error
	}
	defer resp.Body.Close()

	result.Metrics = append(result.Metrics, types.Metric{
		Name: "http.response.alive", Value: 1,
		Labels: map[string]string{"url": h.cfg.URL}, Timestamp: now, Source: "http",
	})
	result.Metrics = append(result.Metrics, types.Metric{
		Name: "http.response.status_code", Value: float64(resp.StatusCode),
		Labels: map[string]string{"url": h.cfg.URL}, Timestamp: now, Source: "http",
	})

	// Parse JSON response and evaluate checks
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("read body: %v", err))
		return result, nil
	}

	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("json parse: %v", err))
		return result, nil
	}

	for _, check := range h.cfg.Checks {
		val, err := extractJSONPath(data, check.JSONPath)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("jsonpath %s: %v", check.JSONPath, err))
			continue
		}
		name := check.Name
		if name == "" {
			name = sanitizeMetricName(check.JSONPath)
		}
		result.Metrics = append(result.Metrics, types.Metric{
			Name:      fmt.Sprintf("http.json.%s", name),
			Value:     val,
			Labels:    map[string]string{"path": check.JSONPath, "url": h.cfg.URL},
			Timestamp: now,
			Source:    "http",
		})
	}

	return result, nil
}

// extractJSONPath handles simple dot-notation paths like ".queue_stats.pending"
func extractJSONPath(data interface{}, path string) (float64, error) {
	path = strings.TrimPrefix(path, ".")
	parts := strings.Split(path, ".")

	current := data
	for _, part := range parts {
		m, ok := current.(map[string]interface{})
		if !ok {
			return 0, fmt.Errorf("expected object at %s", part)
		}
		current, ok = m[part]
		if !ok {
			return 0, fmt.Errorf("key %s not found", part)
		}
	}

	switch v := current.(type) {
	case float64:
		return v, nil
	case json.Number:
		return v.Float64()
	default:
		return 0, fmt.Errorf("value at path is not a number: %T", current)
	}
}
