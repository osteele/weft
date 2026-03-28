package opscore

import "errors"

// ErrNotQueued is returned when an operation requires a queued job but the job
// is in a different state.
var ErrNotQueued = errors.New("job is not queued")

// ErrPauseNotSupported is returned when pause is requested for a backend that
// does not support it (e.g. Slurm).
var ErrPauseNotSupported = errors.New("pause not supported")

// ErrResumeNotSupported is returned when resume is requested for a backend that
// does not support it (e.g. Slurm).
var ErrResumeNotSupported = errors.New("resume not supported")
