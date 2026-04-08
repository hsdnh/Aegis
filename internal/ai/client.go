package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Provider selects which AI backend to use.
type Provider string

const (
	ProviderClaude         Provider = "claude"
	ProviderOpenAI         Provider = "openai"
	ProviderOpenAICompat   Provider = "openai_compatible" // DeepSeek, Ollama, 通义, Azure, OpenRouter, etc.
)

// Client talks to an LLM API.
// Supports: Claude, OpenAI, and any OpenAI-compatible API (DeepSeek, Ollama, 通义千问, etc.)
type Client struct {
	provider   Provider
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
	name       string // human-readable name for logging, e.g. "analyst-claude" or "chat-deepseek"
}

// NewClient creates a client with auto-detected base URL.
func NewClient(provider Provider, apiKey, model string) *Client {
	return NewClientWithURL(provider, apiKey, model, "")
}

// NewClientWithURL creates a client with a custom base URL.
// Use this for DeepSeek, Ollama, 通义千问, Azure OpenAI, OpenRouter, etc.
func NewClientWithURL(provider Provider, apiKey, model, baseURL string) *Client {
	c := &Client{
		provider:   provider,
		apiKey:     apiKey,
		model:      model,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}

	// Auto-detect base URL if not provided
	if c.baseURL == "" {
		switch provider {
		case ProviderClaude:
			c.baseURL = "https://api.anthropic.com/v1/messages"
		case ProviderOpenAI:
			c.baseURL = "https://api.openai.com/v1/chat/completions"
		case ProviderOpenAICompat:
			c.baseURL = "https://api.openai.com/v1/chat/completions" // fallback
		}
	}

	// Auto-detect default model — strongest available per provider
	// User can always override via config `model:` field
	if c.model == "" {
		switch provider {
		case ProviderClaude:
			c.model = "claude-opus-4-20250514"
		case ProviderOpenAI:
			c.model = "o3-2025-04-16"
		case ProviderOpenAICompat:
			c.model = "gpt-4o" // placeholder — user must set actual model name
		}
	}

	// OpenAI-compatible uses OpenAI API format
	if provider == ProviderOpenAICompat {
		// Ensure URL ends with /chat/completions
		if !strings.HasSuffix(c.baseURL, "/chat/completions") {
			c.baseURL = strings.TrimSuffix(c.baseURL, "/") + "/chat/completions"
		}
	}

	return c
}

// SetSystemPrompt is DEPRECATED — use ChatWithSystem instead.
// Kept for backward compat but is a no-op now.
func (c *Client) SetSystemPrompt(prompt string) {
	// no-op: system prompt is now per-request to avoid race conditions
}

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Response from the AI.
type Response struct {
	Content    string `json:"content"`
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	Model       string `json:"model"`
}

// Chat sends a conversation to the AI and returns the response.
func (c *Client) Chat(ctx context.Context, messages []Message) (*Response, error) {
	return c.ChatWithSystem(ctx, "", messages)
}

// ChatWithSystem sends a conversation with a per-request system prompt.
// This is thread-safe — no shared mutable state.
func (c *Client) ChatWithSystem(ctx context.Context, systemPrompt string, messages []Message) (*Response, error) {
	switch c.provider {
	case ProviderClaude:
		return c.chatClaude(ctx, systemPrompt, messages)
	case ProviderOpenAI, ProviderOpenAICompat:
		return c.chatOpenAI(ctx, systemPrompt, messages)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", c.provider)
	}
}

// --- Claude API ---

func (c *Client) chatClaude(ctx context.Context, systemPrompt string, messages []Message) (*Response, error) {
	body := map[string]interface{}{
		"model":      c.model,
		"max_tokens": 4096,
		"messages":   messages,
	}

	if systemPrompt != "" {
		body["system"] = []map[string]interface{}{
			{
				"type": "text",
				"text": systemPrompt,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude API error %d: %s", resp.StatusCode, string(respBody))
	}

	var claudeResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &claudeResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	var content string
	if len(claudeResp.Content) > 0 {
		content = claudeResp.Content[0].Text
	}

	return &Response{
		Content:      content,
		InputTokens:  claudeResp.Usage.InputTokens,
		OutputTokens: claudeResp.Usage.OutputTokens,
		Model:        claudeResp.Model,
	}, nil
}

// --- OpenAI API ---

func (c *Client) chatOpenAI(ctx context.Context, systemPrompt string, messages []Message) (*Response, error) {
	msgs := messages
	if systemPrompt != "" {
		msgs = append([]Message{{Role: "system", Content: systemPrompt}}, msgs...)
	}

	body := map[string]interface{}{
		"model":      c.model,
		"max_tokens": 4096,
		"messages":   msgs,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai API error %d: %s", resp.StatusCode, string(respBody))
	}

	var openaiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	var content string
	if len(openaiResp.Choices) > 0 {
		content = openaiResp.Choices[0].Message.Content
	}

	return &Response{
		Content:      content,
		InputTokens:  openaiResp.Usage.PromptTokens,
		OutputTokens: openaiResp.Usage.CompletionTokens,
		Model:        openaiResp.Model,
	}, nil
}
