package cmd

import (
	"testing"
	"time"
)

func TestParseSinceCutoff_Date(t *testing.T) {
	now := time.Date(2026, 4, 13, 15, 0, 0, 0, time.UTC)
	got, err := parseSinceCutoff("2026-04-01", now)
	if err != nil {
		t.Fatalf("parseSinceCutoff date: %v", err)
	}
	want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("date parse = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestParseSinceCutoff_AgoDuration(t *testing.T) {
	now := time.Date(2026, 4, 13, 15, 0, 0, 0, time.UTC)
	got, err := parseSinceCutoff("36h ago", now)
	if err != nil {
		t.Fatalf("parseSinceCutoff ago: %v", err)
	}
	want := now.Add(-36 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("ago parse = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestParseSinceCutoff_DaysDuration(t *testing.T) {
	now := time.Date(2026, 4, 13, 15, 0, 0, 0, time.UTC)
	got, err := parseSinceCutoff("7d", now)
	if err != nil {
		t.Fatalf("parseSinceCutoff days: %v", err)
	}
	want := now.Add(-7 * 24 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("days parse = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestParseSinceCutoff_RFC3339(t *testing.T) {
	now := time.Date(2026, 4, 13, 15, 0, 0, 0, time.UTC)
	got, err := parseSinceCutoff("2026-04-12T10:30:00Z", now)
	if err != nil {
		t.Fatalf("parseSinceCutoff rfc3339: %v", err)
	}
	want := time.Date(2026, 4, 12, 10, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("rfc3339 parse = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestParseSinceCutoff_Invalid(t *testing.T) {
	now := time.Date(2026, 4, 13, 15, 0, 0, 0, time.UTC)
	if _, err := parseSinceCutoff("yesterday", now); err == nil {
		t.Fatalf("expected parseSinceCutoff to reject invalid value")
	}
}
