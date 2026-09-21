package processguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/db"
)

var ErrBinaryChanged = errors.New("weft binary changed; restart autopilot")

var processBinary = captureProcessBinary()

type BinarySnapshot struct {
	Path        string
	Size        int64
	ModTimeUnix int64
	Dev         uint64
	Ino         uint64
	Valid       bool
}

func captureProcessBinary() BinarySnapshot {
	exe, err := os.Executable()
	if err != nil {
		return BinarySnapshot{}
	}
	return CapturePath(exe)
}

func CapturePath(path string) BinarySnapshot {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	info, err := os.Stat(path)
	if err != nil {
		return BinarySnapshot{}
	}
	snap := BinarySnapshot{
		Path:        path,
		Size:        info.Size(),
		ModTimeUnix: info.ModTime().Unix(),
		Valid:       true,
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		snap.Dev = uint64(st.Dev)
		snap.Ino = uint64(st.Ino)
	}
	return snap
}

func CurrentBinaryIdentity() db.BinaryIdentity {
	return processBinary.Identity()
}

func (s BinarySnapshot) Identity() db.BinaryIdentity {
	if !s.Valid {
		return db.BinaryIdentity{}
	}
	return db.BinaryIdentity{
		Path:        s.Path,
		Size:        s.Size,
		ModTimeUnix: s.ModTimeUnix,
		Dev:         s.Dev,
		Ino:         s.Ino,
	}
}

func EnsureCurrentBinary() error {
	if !processBinary.Valid {
		return nil
	}
	current := CapturePath(processBinary.Path)
	if !current.Valid {
		return fmt.Errorf("stat current executable %s", processBinary.Path)
	}
	if SnapshotsDiffer(processBinary, current) {
		// Name both sides of the mismatch: without the paths, sizes, and
		// mtimes the operator cannot tell which process to restart or
		// whether the new binary is the one they meant to load (wb165).
		return fmt.Errorf("%w: the running process started on %s (size %d, mtime %s) but that path is now size %d, mtime %s; restart to load the on-disk binary",
			ErrBinaryChanged,
			processBinary.Path, processBinary.Size, formatBinaryMtime(processBinary.ModTimeUnix),
			current.Size, formatBinaryMtime(current.ModTimeUnix))
	}
	return nil
}

func formatBinaryMtime(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

func BinaryChanged() (bool, error) {
	if !processBinary.Valid {
		return false, nil
	}
	current := CapturePath(processBinary.Path)
	if !current.Valid {
		return false, fmt.Errorf("stat current executable %s", processBinary.Path)
	}
	return SnapshotsDiffer(processBinary, current), nil
}

func SnapshotsDiffer(a, b BinarySnapshot) bool {
	if !a.Valid || !b.Valid {
		return false
	}
	if a.Dev != 0 || a.Ino != 0 || b.Dev != 0 || b.Ino != 0 {
		if a.Dev != b.Dev || a.Ino != b.Ino {
			return true
		}
	}
	return a.Size != b.Size || a.ModTimeUnix != b.ModTimeUnix
}

func ActiveRunnerBinaryChanged(identity db.BinaryIdentity) (bool, error) {
	if identity.Path == "" {
		return false, nil
	}
	current := CapturePath(identity.Path)
	if !current.Valid {
		return false, fmt.Errorf("stat active runner executable %s", identity.Path)
	}
	return SnapshotsDiffer(BinarySnapshot{
		Path:        identity.Path,
		Size:        identity.Size,
		ModTimeUnix: identity.ModTimeUnix,
		Dev:         identity.Dev,
		Ino:         identity.Ino,
		Valid:       true,
	}, current), nil
}
