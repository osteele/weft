package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
)

const (
	summarizeAnthropicAPIURL     = "https://api.anthropic.com/v1/messages"
	summarizeOpenRouterAPIURL    = "https://openrouter.ai/api/v1/chat/completions"
	summarizeAnthropicAPIVersion = "2023-06-01"
	summarizeTimeout             = 30 * time.Second
	maxLogBytes                  = 4000 // truncate logs to last N bytes per job
)

// summarizeJobLogs calls the configured LLM API to produce a brief summary of job logs.
// Returns empty string if no API key is configured or on error.
func summarizeJobLogs(jobLogs map[int64]string) string {
	if len(jobLogs) == 0 {
		return ""
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Warn("failed to load LLM config for job-log summary", "component", "slack", "error", err)
		return ""
	}
	provider := cfg.LLMProvider()
	key := lookupSummaryAPIKey(provider, cfg.LLM.APIKey)
	if key == "" {
		return ""
	}
	model := cfg.LLMModelForProvider(provider)

	var prompt strings.Builder
	prompt.WriteString("Summarize these job logs from a cloud GPU campaign in 1-3 sentences. ")
	prompt.WriteString("Focus on: what the jobs did, whether they succeeded or failed, and any notable errors. ")
	prompt.WriteString("Be concise — this goes into a Slack notification.\n\n")

	for jobID, logs := range jobLogs {
		truncated := truncateTail(logs, maxLogBytes)
		fmt.Fprintf(&prompt, "=== Job %d ===\n%s\n\n", jobID, truncated)
	}

	summary, err := callSummaryLLM(provider, key, model, prompt.String())
	if err != nil {
		slog.Warn("failed to summarize job logs", "component", "slack", "error", err)
		return ""
	}
	return summary
}

func lookupSummaryAPIKey(provider, configured string) string {
	if strings.EqualFold(strings.TrimSpace(provider), "openrouter") {
		return firstConfigured(
			os.Getenv("OPENROUTER_API_KEY"),
			os.Getenv("CLAUDE_OPENROUTER_API_KEY"),
			os.Getenv("CLAUDEM_OPENROUTER_API_KEY"),
			configured,
			getConfigValue("OPENROUTER_API_KEY", "OPENROUTER_API_KEY"),
			getConfigValue("CLAUDE_OPENROUTER_API_KEY", "CLAUDE_OPENROUTER_API_KEY"),
			getConfigValue("CLAUDEM_OPENROUTER_API_KEY", "CLAUDEM_OPENROUTER_API_KEY"),
		)
	}
	return firstConfigured(
		os.Getenv("ANTHROPIC_API_KEY"),
		configured,
		getConfigValue("ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"),
	)
}

func firstConfigured(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func truncateTail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Take the last maxBytes, find the first newline to avoid cutting mid-line
	tail := s[len(s)-maxBytes:]
	if idx := strings.Index(tail, "\n"); idx >= 0 {
		return "...\n" + tail[idx+1:]
	}
	return "...\n" + tail
}

func callSummaryLLM(provider, apiKey, model, prompt string) (string, error) {
	if strings.EqualFold(strings.TrimSpace(provider), "openrouter") {
		return callOpenRouterSummary(apiKey, model, prompt)
	}
	return callAnthropicSummary(apiKey, model, prompt)
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func callAnthropicSummary(apiKey, model, prompt string) (string, error) {
	reqBody := anthropicRequest{
		Model:     model,
		MaxTokens: 256,
		Messages:  []anthropicMessage{{Role: "user", Content: prompt}},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), summarizeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", summarizeAnthropicAPIURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", summarizeAnthropicAPIVersion)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic API returned %d", resp.StatusCode)
	}

	var result anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("anthropic returned no content")
	}

	return strings.TrimSpace(result.Content[0].Text), nil
}

type openRouterSummaryRequest struct {
	Model     string              `json:"model"`
	MaxTokens int                 `json:"max_tokens"`
	Messages  []openRouterMessage `json:"messages"`
}

type openRouterMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openRouterSummaryResponse struct {
	Choices []struct {
		Message openRouterMessage `json:"message"`
	} `json:"choices"`
}

func callOpenRouterSummary(apiKey, model, prompt string) (string, error) {
	reqBody := openRouterSummaryRequest{
		Model:     model,
		MaxTokens: 256,
		Messages:  []openRouterMessage{{Role: "user", Content: prompt}},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), summarizeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", summarizeOpenRouterAPIURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openrouter API returned %d", resp.StatusCode)
	}

	var result openRouterSummaryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("openrouter returned no choices")
	}

	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}
