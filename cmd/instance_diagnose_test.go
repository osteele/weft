package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestFormatTimelineOffset(t *testing.T) {
	tests := []struct {
		name     string
		base, ts int64
		want     string
	}{
		{"zero", 1000, 1000, "+0s"},
		{"one second", 1000, 1001, "+1s"},
		{"one minute two seconds", 1000, 1062, "+1m2s"},
		{"negative", 1000, 992, "-8s"},
		{"large", 1000, 1000 + 3600 + 120 + 5, "+1h2m5s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTimelineOffset(tt.base, tt.ts)
			if got != tt.want {
				t.Errorf("formatTimelineOffset(%d, %d) = %q, want %q", tt.base, tt.ts, got, tt.want)
			}
		})
	}
}

func TestFormatPhaseDurations(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := formatPhaseDurations(nil); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("full", func(t *testing.T) {
		setupStart := int64(100)
		setupEnd := int64(230)
		runStart := int64(230)
		runEnd := int64(530)
		uploadStart := int64(535)
		uploadEnd := int64(547)
		timings := &db.JobPhaseTimings{
			SetupStart:  &setupStart,
			SetupEnd:    &setupEnd,
			RunStart:    &runStart,
			RunEnd:      &runEnd,
			UploadStart: &uploadStart,
			UploadEnd:   &uploadEnd,
		}
		got := formatPhaseDurations(timings)
		want := "setup 2m10s | run 5m0s | upload 12s"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("setup only", func(t *testing.T) {
		setupStart := int64(100)
		setupEnd := int64(160)
		timings := &db.JobPhaseTimings{
			SetupStart: &setupStart,
			SetupEnd:   &setupEnd,
		}
		got := formatPhaseDurations(timings)
		want := "setup 1m0s"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("no end times", func(t *testing.T) {
		setupStart := int64(100)
		timings := &db.JobPhaseTimings{
			SetupStart: &setupStart,
		}
		if got := formatPhaseDurations(timings); got != "" {
			t.Errorf("expected empty for missing end, got %q", got)
		}
	})
}

func TestFormatGPUMetrics(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := formatGPUMetrics(nil); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("full", func(t *testing.T) {
		peakMem := 23040
		meanUtil := 95
		peakUtil := 100
		timings := &db.JobPhaseTimings{
			PeakGPUMemMiB: &peakMem,
			MeanGPUUtil:   &meanUtil,
			PeakGPUUtil:   &peakUtil,
		}
		got := formatGPUMetrics(timings)
		want := "peak mem 23040 MiB, mean util 95%, peak util 100%"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("partial", func(t *testing.T) {
		peakMem := 4096
		timings := &db.JobPhaseTimings{
			PeakGPUMemMiB: &peakMem,
		}
		got := formatGPUMetrics(timings)
		want := "peak mem 4096 MiB"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestBuildTimeline(t *testing.T) {
	launchedAt := int64(1000)
	endedAt := int64(1900)
	wrapperStart := int64(1050)
	setupStart := int64(1055)
	setupEnd := int64(1200)
	runStart := int64(1200)
	runEnd := int64(1800)

	inst := &db.Launch{
		ID:                launchedAt,
		CreatedAt:         992,
		LaunchedAt:        &launchedAt,
		EndedAt:           &endedAt,
		Status:            db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonBootstrapTimeout,
	}

	jobs := []*db.Job{
		{ID: 42},
	}

	timings := map[int64]*db.JobPhaseTimings{
		42: {
			WrapperStart: &wrapperStart,
			SetupStart:   &setupStart,
			SetupEnd:     &setupEnd,
			RunStart:     &runStart,
			RunEnd:       &runEnd,
		},
	}

	events := []db.LifecycleEvent{
		{OccurredAt: 1900, EventKind: "reconcile.bootstrap_timeout", Detail: "bootstrap timeout"},
	}

	timeline := buildTimeline(inst, jobs, timings, events)

	if len(timeline) == 0 {
		t.Fatal("expected non-empty timeline")
	}

	// Verify ordering: timestamps should be non-decreasing.
	for i := 1; i < len(timeline); i++ {
		if timeline[i].Timestamp < timeline[i-1].Timestamp {
			t.Errorf("timeline not sorted: entry %d (%d) < entry %d (%d)",
				i, timeline[i].Timestamp, i-1, timeline[i-1].Timestamp)
		}
	}

	// First should be created, second launched.
	if timeline[0].Label != "created" {
		t.Errorf("first entry should be 'created', got %q", timeline[0].Label)
	}
	if timeline[1].Label != "launched" {
		t.Errorf("second entry should be 'launched', got %q", timeline[1].Label)
	}

	// Last should be ended.
	last := timeline[len(timeline)-1]
	if last.Timestamp != 1900 {
		t.Errorf("last entry timestamp = %d, want 1900", last.Timestamp)
	}
}

func TestCompareThreshold(t *testing.T) {
	tests := []struct {
		name      string
		elapsed   time.Duration
		warn      time.Duration
		terminate time.Duration
		contains  string
	}{
		{"within", 60 * time.Second, 5 * time.Minute, 10 * time.Minute, "within warn"},
		{"past warn", 400 * time.Second, 5 * time.Minute, 10 * time.Minute, "past warn"},
		{"at terminate", 10 * time.Minute, 5 * time.Minute, 10 * time.Minute, "at terminate"},
		{"exceeded", 700 * time.Second, 5 * time.Minute, 10 * time.Minute, "exceeded terminate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareThreshold(tt.elapsed, tt.warn, tt.terminate)
			if !strings.Contains(got, tt.contains) {
				t.Errorf("compareThreshold(%s, %s, %s) = %q, want containing %q",
					tt.elapsed, tt.warn, tt.terminate, got, tt.contains)
			}
		})
	}
}
