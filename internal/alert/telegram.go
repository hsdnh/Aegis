package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/hsdnh/Aegis/pkg/types"
)

type TelegramConfig struct {
	Token   string  `yaml:"token"`
	ChatIDs []int64 `yaml:"chat_ids"`
}

type TelegramAlerter struct {
	cfg    TelegramConfig
	client *http.Client
}

func NewTelegramAlerter(cfg TelegramConfig) *TelegramAlerter {
	return &TelegramAlerter{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *TelegramAlerter) Name() string { return "telegram" }

func (t *TelegramAlerter) Send(ctx context.Context, alert types.Alert) error {
	text := fmt.Sprintf("*\\[%s\\] %s*\n\n%s\n\n_Source: %s_",
		alert.Severity, escapeMarkdown(alert.Title),
		escapeMarkdown(alert.Body), escapeMarkdown(alert.Source))

	for _, chatID := range t.cfg.ChatIDs {
		payload := map[string]interface{}{
			"chat_id":    chatID,
			"text":       text,
			"parse_mode": "MarkdownV2",
		}
		body, _ := json.Marshal(payload)

		url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.cfg.Token)
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("telegram request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := t.client.Do(req)
		if err != nil {
			return fmt.Errorf("telegram send to %d: %w", chatID, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("telegram returned status %d for chat %d", resp.StatusCode, chatID)
		}
	}
	return nil
}

func escapeMarkdown(s string) string {
	replacer := []string{
		"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]",
		"(", "\\(", ")", "\\)", "~", "\\~", "`", "\\`",
		">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
		"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}",
		".", "\\.", "!", "\\!",
	}
	result := s
	for i := 0; i < len(replacer); i += 2 {
		result = replaceAll(result, replacer[i], replacer[i+1])
	}
	return result
}

func replaceAll(s, old, new string) string {
	var buf bytes.Buffer
	for i := 0; i < len(s); i++ {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			buf.WriteString(new)
			i += len(old) - 1
		} else {
			buf.WriteByte(s[i])
		}
	}
	return buf.String()
}
