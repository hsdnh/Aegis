package alert

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

type BarkConfig struct {
	ServerURL string   `yaml:"server_url"` // default: https://api.day.app
	Keys      []string `yaml:"keys"`
}

type BarkAlerter struct {
	cfg    BarkConfig
	client *http.Client
}

func NewBarkAlerter(cfg BarkConfig) *BarkAlerter {
	if cfg.ServerURL == "" {
		cfg.ServerURL = "https://api.day.app"
	}
	return &BarkAlerter{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (b *BarkAlerter) Name() string { return "bark" }

func (b *BarkAlerter) Send(ctx context.Context, alert types.Alert) error {
	title := fmt.Sprintf("[%s] %s", alert.Severity, alert.Title)
	for _, key := range b.cfg.Keys {
		pushURL := fmt.Sprintf("%s/%s/%s/%s",
			b.cfg.ServerURL,
			url.PathEscape(key),
			url.PathEscape(title),
			url.PathEscape(alert.Body),
		)

		req, err := http.NewRequestWithContext(ctx, "GET", pushURL, nil)
		if err != nil {
			return fmt.Errorf("bark request: %w", err)
		}

		resp, err := b.client.Do(req)
		if err != nil {
			keyHint := key
			if len(keyHint) > 8 {
				keyHint = keyHint[:8]
			}
			return fmt.Errorf("bark send to key %s: %w", keyHint, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("bark returned status %d", resp.StatusCode)
		}
	}
	return nil
}
