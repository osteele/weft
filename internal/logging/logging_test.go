package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/util"
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

func TestSetupTimestampPresentation(t *testing.T) {
	previousLogger, previousLocation := slog.Default(), util.CLITimeLocation()
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		util.SetCLITimeLocation(previousLocation)
	})
	instant := time.Date(2026, 6, 12, 1, 59, 59, 123000000, time.FixedZone("SOURCE", 2*3600))
	display := time.FixedZone("DISPLAY", -4*3600)
	for _, tc := range []struct {
		name, mode string
		location   *time.Location
		want       string
	}{
		{"utc", "text", time.UTC, "2026-06-11 23:59:59.123 UTC"},
		{"named", "text", display, "2026-06-11 19:59:59.123 DISPLAY -04:00"},
		{"json", "json", display, "2026-06-11T23:59:59.123Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			util.SetCLITimeLocation(tc.location)
			var output bytes.Buffer
			logger := Setup(&output, tc.mode)
			record := slog.NewRecord(instant, slog.LevelWarn, "diagnostic", 0)
			record.AddAttrs(slog.Group("source", slog.Time("time", instant)))
			if err := logger.Handler().Handle(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if tc.mode == "json" {
				var event struct {
					Time   string
					Source struct{ Time string }
				}
				if err := json.Unmarshal(output.Bytes(), &event); err != nil {
					t.Fatal(err)
				}
				if event.Time != tc.want {
					t.Errorf("diagnostic time = %q, want %q", event.Time, tc.want)
				}
				if event.Source.Time != "2026-06-12T01:59:59.123+02:00" {
					t.Errorf("source timestamp was rewritten: %q", event.Source.Time)
				}
			} else if !strings.Contains(output.String(), tc.want) {
				t.Errorf("diagnostic missing %q: %s", tc.want, output.String())
			}
		})
	}
}
