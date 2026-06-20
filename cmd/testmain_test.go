package cmd

import (
	"fmt"
	"os"
	"testing"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/ssh"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/spf13/cobra"
)

func TestMain(m *testing.M) {
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
	ensurePredictorUsableFunc = original
	loadPredictorConfig = originalLoadConfig
	os.Exit(code)
}
