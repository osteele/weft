package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunSlackTestRequiresWebhook(t *testing.T) {
	var posted bool
	err := runSlackTest(&bytes.Buffer{}, "", func() string { return "" }, func(string) error {
		posted = true
		return nil
	})
	if err == nil {
		t.Fatal("expected missing webhook to fail")
	}
	if !strings.Contains(err.Error(), "Slack webhook is not configured") {
		t.Fatalf("error = %q, want webhook guidance", err)
	}
	if posted {
		t.Fatal("posted despite missing webhook")
	}
}

func TestRunSlackTestSendsCustomMessage(t *testing.T) {
	var out bytes.Buffer
	var got string
	err := runSlackTest(&out, "hello slack", func() string { return "https://hooks.slack.example/test" }, func(message string) error {
		got = message
		return nil
	})
	if err != nil {
		t.Fatalf("runSlackTest: %v", err)
	}
	if got != "hello slack" {
		t.Fatalf("posted message = %q, want custom message", got)
	}
	if !strings.Contains(out.String(), "Slack test message sent") {
		t.Fatalf("stdout = %q, want success confirmation", out.String())
	}
}

func TestRunSlackTestSurfacesPostFailure(t *testing.T) {
	postErr := errors.New("slack webhook returned 403")
	err := runSlackTest(&bytes.Buffer{}, "", func() string { return "https://hooks.slack.example/test" }, func(string) error {
		return postErr
	})
	if err == nil {
		t.Fatal("expected post failure")
	}
	if !strings.Contains(err.Error(), "send Slack test message") || !strings.Contains(err.Error(), postErr.Error()) {
		t.Fatalf("error = %q, want wrapped post failure", err)
	}
}

func TestDefaultSlackTestMessageIncludesHostAndTime(t *testing.T) {
	now := time.Date(2026, 5, 9, 18, 30, 0, 0, time.UTC)
	got := defaultSlackTestMessage(now, func() (string, error) { return "host-a", nil })
	if !strings.Contains(got, "host-a") || !strings.Contains(got, "2026-05-09T18:30:00Z") {
		t.Fatalf("default message = %q, want host and timestamp", got)
	}
}
