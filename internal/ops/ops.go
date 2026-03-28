// Package ops provides unified job operation functions used by both CLI and TUI.
package ops

import "github.com/osteele/weft/internal/opscore"

// Re-export core types so existing consumers don't need to change imports.
type Result = opscore.Result
type TimeoutMode = opscore.TimeoutMode
type ExecuteOptions = opscore.ExecuteOptions

const (
	TimeoutFast   = opscore.TimeoutFast
	TimeoutNormal = opscore.TimeoutNormal
	TimeoutSync   = opscore.TimeoutSync
)

func DefaultOptions() ExecuteOptions                 { return opscore.DefaultOptions() }
func OptionsForMode(mode TimeoutMode) ExecuteOptions { return opscore.OptionsForMode(mode) }
