package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/ssh"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/spf13/cobra"
)

func TestMain(m *testing.M) {
	dataHome, err := os.MkdirTemp("", "weft-cmd-test-data-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create test data home: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_DATA_HOME", dataHome); err != nil {
		fmt.Fprintf(os.Stderr, "set test data home: %v\n", err)
		os.Exit(1)
	}
	// The shared issue CLI uses its own override, not XDG_DATA_HOME.
	if err := os.Setenv("AGENT_ISSUES_DB", filepath.Join(dataHome, "issues.db")); err != nil {
		fmt.Fprintf(os.Stderr, "set test issue ledger: %v\n", err)
		os.Exit(1)
	}
	originalIssuesCLI := runIssuesCLI
	runIssuesCLI = func(args ...string) error {
		panic(fmt.Sprintf("unexpected issues CLI in cmd test; stub runIssuesCLI for %q", args))
	}
	original := ensurePredictorUsableFunc
	originalLoadConfig := loadPredictorConfig
	ensurePredictorUsableFunc = func(*cobra.Command, *config.Config, string) error {
		return nil
	}
	loadPredictorConfig = func() (*config.Config, error) {
		return nil, nil
	}
	restoreSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		panic(fmt.Sprintf("unexpected SSH in cmd test; stub ssh.SetRunner for host %q command %q", host, command))
	})
	restoreSourceSync := srcsync.SetSyncFunc(func(host, localDir, remoteDir string, _ []string) error {
		panic(fmt.Sprintf("unexpected source sync in cmd test; stub srcsync.SetSyncFunc for %s:%s from %s", host, remoteDir, localDir))
	})
	code := m.Run()
	restoreSourceSync()
	restoreSSH()
	runIssuesCLI = originalIssuesCLI
	ensurePredictorUsableFunc = original
	loadPredictorConfig = originalLoadConfig
	if err := os.RemoveAll(dataHome); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove test data home: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
