package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// VCSInfo records source-control provenance for one source root.
type VCSInfo struct {
	Type     string `json:"type,omitempty"`
	Revision string `json:"revision,omitempty"`
	ChangeID string `json:"change_id,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
}

// DetectVCSInfo returns optional VCS provenance when localDir is recognizably a
// git or jj repo. Source capture remains filesystem-based: this metadata does
// not select files, require commits, or gate snapshots. Failures to run the VCS
// command leave provenance absent rather than inventing an "unknown clean"
// state.
func DetectVCSInfo(localDir string) *VCSInfo {
	if _, err := os.Stat(filepath.Join(localDir, ".jj")); err == nil {
		return detectJjInfo(localDir)
	}
	if _, err := os.Stat(filepath.Join(localDir, ".git")); err == nil {
		return detectGitInfo(localDir)
	}
	return nil
}

func detectJjInfo(localDir string) *VCSInfo {
	out, err := exec.Command("jj", "-R", localDir, "log", "-r", "@", "--no-graph", "-T", `change_id ++ "\n" ++ commit_id ++ "\n" ++ empty ++ "\n"`).Output()
	if err != nil {
		return nil
	}
	return parseJjInfo(string(out))
}

func parseJjInfo(output string) *VCSInfo {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 3 {
		return nil
	}
	empty, err := strconv.ParseBool(strings.TrimSpace(lines[2]))
	if err != nil {
		return nil
	}
	return &VCSInfo{
		Type:     "jj",
		ChangeID: strings.TrimSpace(lines[0]),
		Revision: strings.TrimSpace(lines[1]),
		Dirty:    !empty,
	}
}

func detectGitInfo(localDir string) *VCSInfo {
	out, err := exec.Command("git", "-C", localDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil
	}
	info := &VCSInfo{Type: "git", Revision: strings.TrimSpace(string(out))}
	status, err := exec.Command("git", "-C", localDir, "status", "--porcelain").Output()
	if err != nil || strings.TrimSpace(string(status)) != "" {
		info.Dirty = true
	}
	return info
}
