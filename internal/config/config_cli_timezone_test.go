package config

import (
	"strings"
	"testing"
	"time"
)

func TestCLITimezoneSelectionAcrossDateAndDSTBoundaries(t *testing.T) {
	previousLocal := time.Local
	time.Local = time.FixedZone("TestLocal", 9*60*60)
	t.Cleanup(func() { time.Local = previousLocal })

	winter := time.Date(2026, time.January, 1, 1, 0, 0, 0, time.UTC)
	summer := time.Date(2026, time.July, 1, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, config, winter, summer string
	}{
		{"default", "", "2026-01-01 01:00 UTC +00:00", "2026-07-01 01:00 UTC +00:00"},
		{"local", "cli_timezone = \"local\"", "2026-01-01 10:00 TestLocal +09:00", "2026-07-01 10:00 TestLocal +09:00"},
		{"named", "cli_timezone = \"America/New_York\"", "2025-12-31 20:00 EST -05:00", "2026-06-30 21:00 EDT -04:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadTOMLConfig(t, tc.config)
			if err != nil {
				t.Fatal(err)
			}
			location, err := cfg.CLILocation()
			if err != nil {
				t.Fatal(err)
			}
			const layout = "2006-01-02 15:04 MST -07:00"
			if got := winter.In(location).Format(layout); got != tc.winter {
				t.Errorf("winter display = %q, want %q", got, tc.winter)
			}
			if got := summer.In(location).Format(layout); got != tc.summer {
				t.Errorf("summer display = %q, want %q", got, tc.summer)
			}
		})
	}
}

func TestCLITimezoneRejectsInvalidName(t *testing.T) {
	cfg, err := loadTOMLConfig(t, "cli_timezone = \"America/Not_A_Real_Zone\"")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.CLILocation(); err == nil || !strings.Contains(err.Error(), "America/Not_A_Real_Zone") {
		t.Fatalf("invalid CLI timezone must produce a diagnostic naming the value, got %v", err)
	}
}
