package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultOpenRouterModel   = "anthropic/claude-sonnet-4.6"
	DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
	DefaultOpenRouterTimeout = 60 * time.Second
)

// OpenRouterConfig holds OpenRouter configuration.
type OpenRouterConfig struct {
	APIKey   string
	Model    string
	BaseURL  string
	Timeout  time.Duration
	AppURL   string
	AppTitle string
}

// OpenRouterClient is an OpenRouter API client.
type OpenRouterClient struct {
	config     OpenRouterConfig
	httpClient *http.Client
}

// NewOpenRouterClient creates a new OpenRouter client.
func NewOpenRouterClient(cfg OpenRouterConfig) *OpenRouterClient {
	if cfg.Model == "" {
		cfg.Model = DefaultOpenRouterModel
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultOpenRouterBaseURL
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultOpenRouterTimeout
	}

	return &OpenRouterClient{
		config: cfg,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// IsAvailable returns true if the client is configured with an API key.
func (c *OpenRouterClient) IsAvailable() bool {
	return strings.TrimSpace(c.config.APIKey) != ""
}

// GenerationHash creates a cache key from model and prompt.
func (c *OpenRouterClient) GenerationHash(prompt string) string {
	h := sha256.New()
	h.Write([]byte("openrouter"))
	h.Write([]byte("|"))
	h.Write([]byte(c.config.Model))
	h.Write([]byte("|"))
	h.Write([]byte(c.config.BaseURL))
	h.Write([]byte("|"))
	h.Write([]byte(prompt))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

type openrouterMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openrouterRequest struct {
	Model    string              `json:"model"`
	Messages []openrouterMessage `json:"messages"`
}

type openrouterChoice struct {
	Message openrouterMessage `json:"message"`
}

type openrouterResponse struct {
	Choices []openrouterChoice `json:"choices"`
}

// Generate sends a prompt to OpenRouter and returns the response.
func (c *OpenRouterClient) Generate(ctx context.Context, prompt string) (string, error) {
	if !c.IsAvailable() {
		return "", fmt.Errorf("openrouter API key not configured")
	}

	reqBody := openrouterRequest{
		Model: c.config.Model,
		Messages: []openrouterMessage{{
			Role:    "user",
			Content: prompt,
		}},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := strings.TrimRight(c.config.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	if c.config.AppURL != "" {
		req.Header.Set("HTTP-Referer", c.config.AppURL)
	}
	if c.config.AppTitle != "" {
		req.Header.Set("X-Title", c.config.AppTitle)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openrouter returned status %d", resp.StatusCode)
	}

	var result openrouterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("openrouter returned no choices")
	}

	response := strings.TrimSpace(result.Choices[0].Message.Content)
	return response, nil
}
