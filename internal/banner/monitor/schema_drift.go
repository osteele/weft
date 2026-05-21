// Package monitor implements the background producers that push banners
// onto a banner.Bus. Each monitor runs in its own goroutine and can be
// started independently from a long-running entrypoint (TUI or daemon).
package monitor

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/banner"
	"github.com/osteele/weft/internal/db"
)

// SchemaDriftBannerID is the stable banner ID for schema-drift notices.
// External code that needs to clear or query this banner should reference
// this constant rather than the literal string.
const SchemaDriftBannerID = "schema-drift"

// SchemaDriftConfig configures WatchSchemaDrift.
type SchemaDriftConfig struct {
	// Database is the open *sql.DB to poll.
	Database *sql.DB
	// Bus receives the banner push.
	Bus *banner.Bus
	// Interval between polls. Defaults to 5 minutes.
	Interval time.Duration
	// ProcessStart is the time the running process started. Used to decide
	// whether the on-disk binary is newer than the running one (which
	// implies a relaunch will pick up a fresh schema). Defaults to now.
	ProcessStart time.Time
	// RelaunchSignal, if non-nil, receives an empty struct when schema
	// drift is detected AND the on-disk binary is newer than this process.
	// The receiver is responsible for orderly shutdown and calling
	// ExecSelf. If nil, ExecSelf is invoked directly from the monitor
	// goroutine — which is fine for daemons but inappropriate for TUIs
	// (the terminal stays in alt-screen / raw mode).
	RelaunchSignal chan<- struct{}
}

// WatchSchemaDrift polls the database's applied migration version every
// cfg.Interval and reacts to drift:
//
//   - If the DB version is ahead of this binary AND the on-disk binary has
//     been modified since this process started, the binary on disk is
//     "newer" and re-execing it will let it pick up the schema. Calls
//     cfg.OnRelaunchEligible (or ExecSelf directly).
//
//   - If the DB version is ahead BUT the on-disk binary is not newer,
//     re-exec would just hit the same drift. Push a banner so the user
//     knows the situation is stuck and needs manual intervention.
//
// Runs until ctx is cancelled. All errors are logged at debug level —
// transient read failures should not produce user-visible noise.
func WatchSchemaDrift(ctx context.Context, cfg SchemaDriftConfig) {
	if cfg.Database == nil || cfg.Bus == nil {
		return
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	processStart := cfg.ProcessStart
	if processStart.IsZero() {
		processStart = time.Now()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			checkSchemaDriftOnce(cfg, processStart)
		}
	}
}

func checkSchemaDriftOnce(cfg SchemaDriftConfig, processStart time.Time) {
	current, observed, err := db.ObservedSchemaVersions(cfg.Database)
	if err != nil {
		slog.Debug("schema drift poll failed", "component", "banner", "error", err)
		return
	}
	if observed <= current {
		// No drift, or binary is newer than DB. Either way, clear any
		// previous drift banner — the situation has resolved.
		cfg.Bus.Clear(SchemaDriftBannerID)
		return
	}
	newer, err := IsBinaryNewerThan(processStart)
	if err != nil {
		slog.Warn("schema drift detected; could not stat binary", "component", "banner", "error", err)
	}
	if newer {
		slog.Info("schema drift detected and on-disk binary is newer; relaunching",
			"component", "banner", "db_version", observed, "binary_version", current)
		cfg.Bus.Clear(SchemaDriftBannerID)
		if cfg.RelaunchSignal != nil {
			// Non-blocking send: if the host already has a pending signal,
			// dropping the duplicate is fine — they'll relaunch from the
			// first one.
			select {
			case cfg.RelaunchSignal <- struct{}{}:
			default:
			}
			return
		}
		if err := ExecSelf(); err != nil {
			slog.Warn("ExecSelf failed during schema-drift relaunch",
				"component", "banner", "error", err)
		}
		return
	}
	cfg.Bus.Push(banner.Banner{
		ID:       SchemaDriftBannerID,
		Severity: banner.SeverityCritical,
		Text: fmt.Sprintf(
			"DB schema is at v%d; this binary expects v%d. Restart will not help.",
			observed, current,
		),
		Hint: "upgrade weft (the running binary is older than the database)",
	})
}

// IsBinaryNewerThan reports whether the executable backing this process
// has been modified after the given reference time. Returns true when the
// on-disk binary's mtime is strictly after ref. A negative answer (or an
// error) means a relaunch wouldn't pick up newer code.
func IsBinaryNewerThan(ref time.Time) (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("os.Executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err == nil {
		exe = resolved
	}
	info, err := os.Stat(exe)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", exe, err)
	}
	return info.ModTime().After(ref), nil
}

// ExecSelf replaces the current process image with a fresh exec of the
// same binary, preserving argv and the environment. Returns an error only
// if the exec itself fails (which is rare — usually the function does not
// return).
func ExecSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
