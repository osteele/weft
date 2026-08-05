package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// VCSInfo records source-control provenance for one source root.
type VCSInfo struct {
	Type     string `json:"type,omitempty"`
	Revision string `json:"revision,omitempty"`
	ChangeID string `json:"change_id,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
}

// DetectVCSInfo returns VCS provenance when localDir is recognizably a git or
// jj repo. Failures to run the VCS command leave provenance absent rather than
// inventing an "unknown clean" state.
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
	out, err := exec.Command("jj", "-R", localDir, "log", "-r", "@", "--no-graph", "-T", `change_id ++ "\n" ++ commit_id ++ "\n"`).Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	info := &VCSInfo{Type: "jj"}
	if len(lines) > 0 {
		info.ChangeID = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		info.Revision = strings.TrimSpace(lines[1])
	}
	if err := exec.Command("jj", "-R", localDir, "diff", "--quiet").Run(); err != nil {
		info.Dirty = true
	}
	return info
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
