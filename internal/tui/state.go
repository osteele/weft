package tui

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// State holds persisted TUI state that should survive across sessions
type State struct {
	// HostFilter is the last selected host filter (empty means "recent")
	HostFilter string `yaml:"host_filter,omitempty"`
}

var statePath string

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	statePath = filepath.Join(home, ".config", "remote-jobs", "tui-state.yaml")
}

// LoadState reads the TUI state file, returning empty state if it doesn't exist
func LoadState() *State {
	state := &State{}

	if statePath == "" {
		return state
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		return state
	}

	_ = yaml.Unmarshal(data, state)
	return state
}

// SaveState writes the TUI state to disk
func SaveState(state *State) error {
	if statePath == "" {
		return nil
	}

	// Ensure directory exists
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(state)
	if err != nil {
		return err
	}

	return os.WriteFile(statePath, data, 0644)
}

// saveHostFilter saves the current host filter to state
func (m *Model) saveHostFilter() {
	state := &State{}
	if m.jobHostFilterMode == hostFilterSpecific && m.jobHostFilterHost != "" {
		state.HostFilter = m.jobHostFilterHost
	}
	// Ignore errors - this is best-effort persistence
	_ = SaveState(state)
}
