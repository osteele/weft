package cmd

import "testing"

func TestResolveCampaignTUIMode(t *testing.T) {
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
			got, err := resolveCampaignTUIMode(tt.forceTUI, tt.forcePlain, tt.hasTerminal, tt.agentCtx)
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
