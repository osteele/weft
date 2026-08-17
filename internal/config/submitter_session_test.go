package config

import "testing"

func envFrom(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

func TestSubmitterSessionPrefersNativeIDOverLauncherID(t *testing.T) {
	// The nesting protection: a launcher-minted AGENT_SESSION_ID must not win
	// over an id the agent minted for itself, which is more specific.
	got := submitterSessionFrom(defaultSubmitterSessionEnvVars, envFrom(map[string]string{
		"CLAUDE_CODE_SESSION_ID": "claude-1",
		"AGENT_SESSION_ID":       "launcher-9",
	}))
	if got != "claude-1" {
		t.Errorf("SubmitterSession = %q, want %q", got, "claude-1")
	}
}

func TestSubmitterSessionFallsThroughEmptyAndBlankValues(t *testing.T) {
	got := submitterSessionFrom(defaultSubmitterSessionEnvVars, envFrom(map[string]string{
		"CLAUDE_CODE_SESSION_ID": "",
		"CODEX_THREAD_ID":        "   ",
		"AGENT_SESSION_ID":       "launcher-9",
	}))
	if got != "launcher-9" {
		t.Errorf("SubmitterSession = %q, want %q", got, "launcher-9")
	}
}

func TestSubmitterSessionEmptyWhenNothingExported(t *testing.T) {
	if got := submitterSessionFrom(defaultSubmitterSessionEnvVars, envFrom(nil)); got != "" {
		t.Errorf("SubmitterSession = %q, want empty", got)
	}
}

func TestSubmitterSessionTrimsValue(t *testing.T) {
	got := submitterSessionFrom(defaultSubmitterSessionEnvVars, envFrom(map[string]string{
		"CLAUDE_CODE_SESSION_ID": "  claude-1\n",
	}))
	if got != "claude-1" {
		t.Errorf("SubmitterSession = %q, want %q", got, "claude-1")
	}
}

func TestSubmitterSessionEnvVarsDefaultOrder(t *testing.T) {
	var c *Config
	want := []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "AGENT_SESSION_ID"}
	for _, cfg := range []*Config{c, {}} {
		got := cfg.SubmitterSessionEnvVars()
		if len(got) != len(want) {
			t.Fatalf("SubmitterSessionEnvVars = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("SubmitterSessionEnvVars = %v, want %v", got, want)
			}
		}
	}
}

// A configured list replaces the defaults rather than extending them: the user
// is expressing an order, and appending defaults after it would resurrect vars
// they meant to exclude.
func TestSubmitterSessionEnvVarsConfiguredListReplacesDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.Notifications.SubmitterSessionEnvVars = []string{" MY_AGENT_ID ", "", "OTHER_ID"}
	got := cfg.SubmitterSessionEnvVars()
	want := []string{"MY_AGENT_ID", "OTHER_ID"}
	if len(got) != len(want) {
		t.Fatalf("SubmitterSessionEnvVars = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SubmitterSessionEnvVars = %v, want %v", got, want)
		}
	}
}

func TestSubmitterSessionEnvVarsAllBlankFallsBackToDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.Notifications.SubmitterSessionEnvVars = []string{"", "  "}
	if got := cfg.SubmitterSessionEnvVars(); len(got) != len(defaultSubmitterSessionEnvVars) {
		t.Errorf("SubmitterSessionEnvVars = %v, want the defaults", got)
	}
}
