package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicAPIURL     = "https://api.anthropic.com/v1/messages"
	anthropicModel      = "claude-sonnet-4-20250514"
	anthropicAPIVersion = "2023-06-01"
	summarizeTimeout    = 30 * time.Second
	maxLogBytes         = 4000 // truncate logs to last N bytes per job
)

// getAnthropicKey returns the Anthropic API key from environment or config file.
func getAnthropicKey() string {
	return getConfigValue("ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY")
}

// summarizeJobLogs calls the Anthropic API to produce a brief summary of job logs.
// Returns empty string if no API key is configured or on error.
func summarizeJobLogs(jobLogs map[int64]string) string {
	key := getAnthropicKey()
	if key == "" {
		return ""
	}

	if len(jobLogs) == 0 {
		return ""
	}

	var prompt strings.Builder
	prompt.WriteString("Summarize these job logs from a cloud GPU campaign in 1-3 sentences. ")
	prompt.WriteString("Focus on: what the jobs did, whether they succeeded or failed, and any notable errors. ")
	prompt.WriteString("Be concise — this goes into a Slack notification.\n\n")

	for jobID, logs := range jobLogs {
		truncated := truncateTail(logs, maxLogBytes)
		fmt.Fprintf(&prompt, "=== Job %d ===\n%s\n\n", jobID, truncated)
	}

	summary, err := callAnthropic(key, prompt.String())
	if err != nil {
		log.Printf("slack: summarize job logs: %v", err)
		return ""
	}
	return summary
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

func callAnthropic(apiKey, prompt string) (string, error) {
	reqBody := anthropicRequest{
		Model:     anthropicModel,
		MaxTokens: 256,
		Messages:  []anthropicMessage{{Role: "user", Content: prompt}},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), summarizeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)

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
