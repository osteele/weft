package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestSetupRedactsMessagesAndAttributes(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	var output bytes.Buffer
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	logger := Setup(&output, "text")
	logger.Warn("request token "+secret+" failed",
		"url", "https://example.test/path?api_key="+secret,
		"error", errors.New("Authorization: Bearer "+secret))

	got := output.String()
	if strings.Contains(got, secret) {
		t.Fatalf("log output contains credential: %q", got)
	}
	if count := strings.Count(got, "<redacted>"); count != 3 {
		t.Fatalf("log output has %d redactions, want 3: %q", count, got)
	}
}
