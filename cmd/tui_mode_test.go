package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTUIMode(t *testing.T) {
	tests := []struct {
		name        string
		forceTUI    bool
		forcePlain  bool
		hasTerminal bool
		agentCtx    bool
		wantTUI     bool
		wantErr     bool
	}{
		{
			name:        "defaults to tui in interactive terminal",
			hasTerminal: true,
			wantTUI:     true,
		},
		{
			name:        "defaults to plain in non terminal",
			hasTerminal: false,
			wantTUI:     false,
		},
		{
			name:        "defaults to plain in agent context",
			hasTerminal: true,
			agentCtx:    true,
			wantTUI:     false,
		},
		{
			name:        "force tui",
			forceTUI:    true,
			hasTerminal: true,
			agentCtx:    true,
			wantTUI:     true,
		},
		{
			name:        "force tui without terminal errors",
			forceTUI:    true,
			wantErr:     true,
			wantTUI:     false,
			agentCtx:    true,
			hasTerminal: false,
		},
		{
			name:        "force plain",
			forcePlain:  true,
			hasTerminal: true,
			wantTUI:     false,
		},
		{
			name:        "force flags conflict",
			forceTUI:    true,
			forcePlain:  true,
			hasTerminal: true,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTUIMode(tt.forceTUI, tt.forcePlain, tt.hasTerminal, tt.agentCtx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.wantTUI {
				t.Fatalf("useTUI = %v, want %v", got, tt.wantTUI)
			}
		})
	}
}

func TestIsTruthyEnv(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: "0", want: false},
		{value: "false", want: false},
		{value: "off", want: false},
		{value: "no", want: false},
		{value: "1", want: true},
		{value: "true", want: true},
		{value: "yes", want: true},
		{value: "enabled", want: true},
	}

	for _, tt := range tests {
		if got := isTruthyEnv(tt.value); got != tt.want {
			t.Errorf("isTruthyEnv(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestInAgentContextWithTerminal(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_CI", "")
	t.Setenv("GEMINI_CLI", "")

	t.Setenv("CODEX_CI", "1")
	if got := inAgentContextWithTerminal(true); got {
		t.Fatal("expected CODEX_CI in interactive terminal to not force agent context")
	}
	if got := inAgentContextWithTerminal(false); !got {
		t.Fatal("expected CODEX_CI without terminal to force agent context")
	}

	t.Setenv("CLAUDECODE", "1")
	if got := inAgentContextWithTerminal(true); !got {
		t.Fatal("expected CLAUDECODE to force agent context even in terminal")
	}
}

func TestResolveTUIJobScopeRequiresCompleteReadOnlyScope(t *testing.T) {
	scope, err := resolveTUIJobScope("", "", false)
	if err != nil || scope != nil {
		t.Fatalf("default scope = (%+v, %v), want (nil, nil)", scope, err)
	}

	for _, tc := range []struct {
		name     string
		session  string
		project  string
		readOnly bool
	}{
		{name: "session only", session: "session-a"},
		{name: "project only", project: "/tmp/project"},
		{name: "read only only", readOnly: true},
		{name: "missing read only", session: "session-a", project: "/tmp/project"},
		{name: "relative project", session: "session-a", project: "project", readOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveTUIJobScope(tc.session, tc.project, tc.readOnly); err == nil {
				t.Fatal("resolveTUIJobScope returned nil error")
			}
		})
	}

	scope, err = resolveTUIJobScope(" session-a ", "/tmp/project/../project", true)
	if err != nil {
		t.Fatal(err)
	}
	if scope.SubmitterSession != "session-a" || scope.ProjectRoot != "/tmp/project" {
		t.Fatalf("scope = %+v", scope)
	}

	realProject := t.TempDir()
	projectAlias := filepath.Join(t.TempDir(), "project-alias")
	if err := os.Symlink(realProject, projectAlias); err != nil {
		t.Fatal(err)
	}
	scope, err = resolveTUIJobScope("session-a", projectAlias, true)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.EvalSymlinks(realProject)
	if err != nil {
		t.Fatal(err)
	}
	if scope.ProjectRoot != wantRoot {
		t.Fatalf("symlinked project root = %q, want %q", scope.ProjectRoot, wantRoot)
	}
}
