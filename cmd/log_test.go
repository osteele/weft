package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/spf13/pflag"
)

func TestJobLogCmdRegistersSameFlagsAsLogCmd(t *testing.T) {
	logCmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if got := jobLogCmd.Flags().Lookup(flag.Name); got == nil {
			t.Errorf("job log missing flag %q", flag.Name)
		}
	})
}

func TestJobLogArgsAllowOpsModeWithoutJobID(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	logOps = true
	if err := jobLogCmd.Args(jobLogCmd, nil); err != nil {
		t.Fatalf("jobLogCmd.Args returned %v, want nil", err)
	}
}

func TestReadOpsEntriesIncludesSyncedInstanceLogs(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	home := t.TempDir()
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}

	if err := oplog.Init(oplog.DefaultLogPath(), 0); err != nil {
		t.Fatalf("oplog.Init: %v", err)
	}
	oplog.Log(oplog.OpCLICommand, oplog.WithDetail("local"))
	if err := oplog.Close(); err != nil {
		t.Fatalf("oplog.Close: %v", err)
	}

	instanceID := int64(17)
	instancePath := oplog.SyncedInstanceLogPath(instanceID)
	if err := os.MkdirAll(filepath.Dir(instancePath), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	instanceEntries := []byte(`{"t":"2026-01-02T03:04:05Z","op":"agent.start","detail":"remote"}` + "\n")
	if err := os.WriteFile(instancePath, instanceEntries, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := readOpsEntries()
	if err != nil {
		t.Fatalf("readOpsEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("readOpsEntries returned %d entries, want 2", len(entries))
	}

	foundRemote := false
	for _, entry := range entries {
		if entry.Detail == "remote" {
			foundRemote = true
			if entry.Host != db.LaunchHost(instanceID) {
				t.Fatalf("remote entry host = %q, want %q", entry.Host, db.LaunchHost(instanceID))
			}
		}
	}
	if !foundRemote {
		t.Fatal("did not find synced instance ops log entry")
	}
}

func resetLogModeState() {
	logFollow = false
	logLines = 50
	logFrom = 0
	logTo = 0
	logGrep = ""
	logFull = false
	logTimeout = 0
	logSync = false
	logNoSync = false
	logOps = false
	logOpsJob = 0
	logOpsHost = ""
	logOpsOp = ""
	logOpsSince = ""
	logOpsErrors = false
	logEvents = false
	logEventsKind = ""
	logEventsLaunch = 0
	logEventsStats = false
}
