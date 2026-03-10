package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// httpClient is shared across the package. Individual calls use context
// timeouts; the client timeout is a safety net for calls without a context.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// Post sends a text message to the configured Slack webhook.
// Returns nil if no webhook is configured.
func Post(message string) error {
	webhook := GetWebhook()
	if webhook == "" {
		return nil
	}

	payload, err := json.Marshal(map[string]string{"text": message})
	if err != nil {
		return fmt.Errorf("marshal slack payload: %w", err)
	}

	resp, err := httpClient.Post(webhook, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("post to slack: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
	}
	return nil
}
