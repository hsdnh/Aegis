package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Provider selects which AI backend to use.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderOpenAI Provider = "openai"
)

// Client talks to an LLM API. Supports Claude and OpenAI.
type Client struct {
	provider   Provider
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client

	// Prompt caching: static context sent once, reused across calls.
	// Saves ~80% input tokens when system prompt + project context is large.
	cachedSystemPrompt string
}

func NewClient(provider Provider, apiKey, model string) *Client {
	c := &Client{
		provider:   provider,
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
	switch provider {
	case ProviderClaude:
		c.baseURL = "https://api.anthropic.com/v1/messages"
		if model == "" {
			c.model = "claude-sonnet-4-20250514"
		}
	case ProviderOpenAI:
		c.baseURL = "https://api.openai.com/v1/chat/completions"
		if model == "" {
			c.model = "gpt-4o"
		}
	}
	return c
}

// SetSystemPrompt sets the cached system prompt (static context).
func (c *Client) SetSystemPrompt(prompt string) {
	c.cachedSystemPrompt = prompt
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
	switch c.provider {
	case ProviderClaude:
		return c.chatClaude(ctx, messages)
	case ProviderOpenAI:
		return c.chatOpenAI(ctx, messages)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", c.provider)
	}
}

// --- Claude API ---

func (c *Client) chatClaude(ctx context.Context, messages []Message) (*Response, error) {
	// Build request body
	body := map[string]interface{}{
		"model":      c.model,
		"max_tokens": 4096,
		"messages":   messages,
	}

	// System prompt with cache control (Anthropic prompt caching)
	if c.cachedSystemPrompt != "" {
		body["system"] = []map[string]interface{}{
			{
				"type": "text",
				"text": c.cachedSystemPrompt,
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

func (c *Client) chatOpenAI(ctx context.Context, messages []Message) (*Response, error) {
	msgs := messages
	if c.cachedSystemPrompt != "" {
		msgs = append([]Message{{Role: "system", Content: c.cachedSystemPrompt}}, msgs...)
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
