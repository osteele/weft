package config

import "testing"

// The TUIs have always shipped with mouse reporting on; the default preserves
// that behavior now that EnableMouse is wired to the TUI startup paths.
func TestDefaultConfigEnablesMouse(t *testing.T) {
	if !DefaultConfig().EnableMouse {
		t.Fatal("EnableMouse default must be true to preserve the TUIs' existing mouse-on behavior")
	}
}
