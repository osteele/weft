package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/sessioninbox"
	"github.com/spf13/cobra"
)

func TestSessionQueryPreservesMachineStdoutPurity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-purity")
	t.Setenv("CODEX_THREAD_ID", "less-specific-session")
	configDir := t.TempDir()
	t.Cleanup(config.SetConfigPathsForTesting(filepath.Join(configDir, "config.toml"), filepath.Join(configDir, "config.yaml")))
	database := db.SetupTestDB(t)
	stubEmptyQueueStatus(t)

	visibleID, err := db.RecordQueued(database, "cool30", "/tmp/purity", "echo visible", "visible")
	if err != nil {
		t.Fatal(err)
	}
	visible, err := db.GetJobByID(database, visibleID)
	if err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "result.txt")
	if err := os.WriteFile(source, []byte("artifact bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.StoreLocalArtifact(database, visibleID, "output/result.txt", source); err != nil {
		t.Fatal(err)
	}

	oldArtifactOutput, oldArtifactAll := artifactOutput, artifactAll
	oldArtifactTag, oldArtifactLatest := artifactTag, artifactLatest
	t.Cleanup(func() {
		artifactOutput, artifactAll = oldArtifactOutput, oldArtifactAll
		artifactTag, artifactLatest = oldArtifactTag, oldArtifactLatest
	})
	artifactAll = false
	artifactTag = nil
	artifactLatest = false

	renderList := func(format string) string {
		t.Helper()
		restoreListFlags(t)
		listFormat = format
		listColumns = []string{"id", "status"}
		return captureStdout(t, func() {
			if err := printJobs(database, []*db.Job{visible}); err != nil {
				t.Fatalf("printJobs(%s): %v", format, err)
			}
		})
	}
	renderCat := func() string {
		t.Helper()
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		if err := runArtifactCat(cmd, []string{strconv.FormatInt(visibleID, 10), "output/result.txt"}); err != nil {
			t.Fatalf("artifact cat: %v", err)
		}
		return out.String()
	}
	renderGetStream := func() string {
		t.Helper()
		artifactOutput = "-"
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		if err := runArtifactGet(cmd, []string{strconv.FormatInt(visibleID, 10), "output/result.txt"}); err != nil {
			t.Fatalf("artifact get -: %v", err)
		}
		return out.String()
	}

	without := map[string]string{
		"json":         renderList("json"),
		"tsv":          renderList("tsv"),
		"artifact-cat": renderCat(),
		"artifact-get": renderGetStream(),
	}

	inboxID, err := db.RecordQueued(database, "cool30", "/tmp/purity", "echo done", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, inboxID, "session-purity"); err != nil {
		t.Fatal(err)
	}
	exitZero := 0
	if err := db.CloseAttempt(database, inboxID, db.StatusCompleted, &exitZero, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	with := map[string]string{
		"json":         renderList("json"),
		"tsv":          renderList("tsv"),
		"artifact-cat": renderCat(),
		"artifact-get": renderGetStream(),
	}
	for name, want := range without {
		if got := with[name]; got != want {
			t.Errorf("%s stdout changed when a session inbox row appeared\nwithout: %q\nwith:    %q", name, want, got)
		}
	}

	oldProject := sessionUnprocessedProject
	sessionUnprocessedProject = ""
	t.Cleanup(func() { sessionUnprocessedProject = oldProject })
	var queryOut bytes.Buffer
	queryCmd := &cobra.Command{}
	queryCmd.SetOut(&queryOut)
	if err := runSessionUnprocessed(queryCmd, nil); err != nil {
		t.Fatalf("runSessionUnprocessed: %v", err)
	}
	var query sessioninbox.Query
	if err := json.Unmarshal(queryOut.Bytes(), &query); err != nil {
		t.Fatalf("query JSON: %v\n%s", err, queryOut.String())
	}
	if query.Version != 1 || query.Scope.State != sessioninbox.ScopeScopedNonEmpty || query.Counts.Total != 1 {
		t.Fatalf("query = %+v", query)
	}
}

func TestHumanCommandsRenderSessionReminderAtReachableSeams(t *testing.T) {
	stubEmptyQueueStatus(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-human-surfaces")
	configDir := t.TempDir()
	t.Cleanup(config.SetConfigPathsForTesting(filepath.Join(configDir, "config.toml"), filepath.Join(configDir, "config.yaml")))
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "cool30", "/tmp/reminder", "echo done", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, jobID, "session-human-surfaces"); err != nil {
		t.Fatal(err)
	}
	exitZero := 0
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exitZero, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := logcache.Write(jobID, "finished\n"); err != nil {
		t.Fatal(err)
	}
	jobArg := strconv.FormatInt(jobID, 10)
	const want = "This session has 1 unprocessed terminal job"

	restoreStatusFlags(t)
	statusNoSync = true
	statusOut := captureStdout(t, func() {
		if err := runJobStatus(&cobra.Command{}, nil); err != nil {
			t.Fatalf("status: %v", err)
		}
	})
	if !strings.Contains(statusOut, want) {
		t.Errorf("status output missing reminder:\n%s", statusOut)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true
	infoOut := captureStdout(t, func() {
		if err := runInfo(&cobra.Command{}, []string{jobArg}); err != nil {
			t.Fatalf("info: %v", err)
		}
	})
	if !strings.Contains(infoOut, want) {
		t.Errorf("info output missing reminder:\n%s", infoOut)
	}

	resetLogModeState()
	t.Cleanup(resetLogModeState)
	logNoSync = true
	var logErr bytes.Buffer
	logCmd := &cobra.Command{}
	logCmd.SetErr(&logErr)
	captureStdout(t, func() {
		if err := runLog(logCmd, []string{jobArg}); err != nil {
			t.Fatalf("log: %v", err)
		}
	})
	if !strings.Contains(logErr.String(), want) {
		t.Errorf("log stderr missing reminder:\n%s", logErr.String())
	}

	oldBuild := buildArtifactR2Client
	oldListSync := artifactListSync
	buildArtifactR2Client = func() cloudOutputStore { return nil }
	artifactListSync = false
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		artifactListSync = oldListSync
	})
	var artifactOut bytes.Buffer
	artifactCmd := &cobra.Command{}
	artifactCmd.SetOut(&artifactOut)
	if err := runArtifactList(artifactCmd, []string{jobArg}); err != nil {
		t.Fatalf("artifact list: %v", err)
	}
	if !strings.Contains(artifactOut.String(), want) {
		t.Errorf("artifact list output missing reminder:\n%s", artifactOut.String())
	}
}
