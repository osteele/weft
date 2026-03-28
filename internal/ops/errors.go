package ops

import "github.com/osteele/weft/internal/opscore"

// Re-export error variables from opscore.
var ErrNotQueued = opscore.ErrNotQueued
var ErrPauseNotSupported = opscore.ErrPauseNotSupported
var ErrResumeNotSupported = opscore.ErrResumeNotSupported
