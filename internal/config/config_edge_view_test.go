package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadTOMLConfig(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	origPath, origLegacy := configPath, legacyConfigPath
	configPath, legacyConfigPath = path, ""
	defer func() { configPath, legacyConfigPath = origPath, origLegacy }()
	return Load()
}

func TestEdgeViewConfigParsesAndDefaults(t *testing.T) {
	cfg, err := loadTOMLConfig(t, `
[edge]
role = "edge"

[edge.view]
bucket = "weft-edge-view"
account_id = "acct"
access_key_id = "key"
secret_access_key = "secret"
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Edge.View.Bucket != "weft-edge-view" {
		t.Fatalf("bucket = %q", cfg.Edge.View.Bucket)
	}
	if cfg.Edge.View.R2().AccessKeyID != "key" {
		t.Fatalf("access key = %q", cfg.Edge.View.R2().AccessKeyID)
	}
	if got := cfg.Edge.View.PublishInterval(); got != DefaultEdgeViewPublishInterval {
		t.Fatalf("publish interval = %s, want default %s", got, DefaultEdgeViewPublishInterval)
	}
	if got := cfg.Edge.View.StaleAfter(); got != DefaultEdgeViewStaleAfter {
		t.Fatalf("stale after = %s, want default %s", got, DefaultEdgeViewStaleAfter)
	}
	if UnknownTOMLKeys != nil {
		t.Fatalf("edge.view keys reported unknown: %v", UnknownTOMLKeys)
	}
}

func TestEdgeViewConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "zero publish interval refused",
			body:    "[edge.view]\nbucket = \"b\"\npublish_interval_seconds = 0\n",
			wantErr: "edge.view.publish_interval_seconds",
		},
		{
			name:    "zero stale bound refused",
			body:    "[edge.view]\nbucket = \"b\"\nstale_after_minutes = 0\n",
			wantErr: "edge.view.stale_after_minutes",
		},
		{
			name:    "bound not exceeding interval refused",
			body:    "[edge.view]\nbucket = \"b\"\npublish_interval_seconds = 600\nstale_after_minutes = 5\n",
			wantErr: "edge.view.stale_after_minutes",
		},
		{
			name:    "coherent explicit values accepted",
			body:    "[edge.view]\nbucket = \"b\"\npublish_interval_seconds = 30\nstale_after_minutes = 5\n",
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadTOMLConfig(t, tc.body)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if got := cfg.Edge.View.PublishInterval(); got != 30*time.Second {
					t.Fatalf("publish interval = %s", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("config with %s accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name %s", err, tc.wantErr)
			}
		})
	}
}

func TestEdgeViewAbsentSkipsValidation(t *testing.T) {
	cfg, err := loadTOMLConfig(t, "[edge]\nrole = \"hub\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Edge.View.Configured() {
		t.Fatal("absent [edge.view] reported configured")
	}
}

func TestEdgeSubmissionIntervalsParseDefaultAndValidate(t *testing.T) {
	cfg, err := loadTOMLConfig(t, "[edge]\nrole = \"edge\"\nadmission_wait_seconds = 7\npoll_interval_seconds = 3\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Edge.AdmissionWait(); got != 7*time.Second {
		t.Fatalf("admission wait = %s", got)
	}
	if got := cfg.Edge.PollInterval(); got != 3*time.Second {
		t.Fatalf("poll interval = %s", got)
	}

	defaultCfg, err := loadTOMLConfig(t, "[edge]\nrole = \"edge\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if defaultCfg.Edge.AdmissionWait() != DefaultEdgeAdmissionWait || defaultCfg.Edge.PollInterval() != DefaultEdgePollInterval {
		t.Fatalf("defaults = %s, %s", defaultCfg.Edge.AdmissionWait(), defaultCfg.Edge.PollInterval())
	}
	expectCfg, err := loadTOMLConfig(t, "[edge.expect.job]\ntypical_minutes = 2.5\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := expectCfg.Edge.AdmissionWait(); got != 150*time.Second {
		t.Fatalf("admission wait from job expectation = %s, want 2m30s", got)
	}

	if _, err := loadTOMLConfig(t, "[edge]\npoll_interval_seconds = 0\n"); err == nil || !strings.Contains(err.Error(), "poll_interval_seconds") {
		t.Fatalf("zero poll interval error = %v", err)
	}
	if _, err := loadTOMLConfig(t, "[edge]\nadmission_wait_seconds = 0\n"); err == nil || !strings.Contains(err.Error(), "admission_wait_seconds") {
		t.Fatalf("zero admission wait error = %v", err)
	}
}
