package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/osteele/weft/internal/logcache"
	"github.com/spf13/cobra"
)

func setLogEdgeFlags(t *testing.T, lines int) {
	t.Helper()
	savedFollow, savedLines, savedFrom, savedTo := logFollow, logLines, logFrom, logTo
	savedGrep, savedFull, savedTimeout := logGrep, logFull, logTimeout
	savedAttempt, savedSync, savedNoSync := logAttempt, logSync, logNoSync
	t.Cleanup(func() {
		logFollow, logLines, logFrom, logTo = savedFollow, savedLines, savedFrom, savedTo
		logGrep, logFull, logTimeout = savedGrep, savedFull, savedTimeout
		logAttempt, logSync, logNoSync = savedAttempt, savedSync, savedNoSync
	})
	logFollow, logLines, logFrom, logTo = false, lines, 0, 0
	logGrep, logFull, logTimeout = "", false, 0
	logAttempt, logSync, logNoSync = 0, false, true
}

// TestLastBytesPinsTailBoundary kills off-by-one mutations at both sides of
// the 256 KiB publication limit.
func TestLastBytesPinsTailBoundary(t *testing.T) {
	at := bytes.Repeat([]byte{'a'}, edgeview.LogTailBytes)
	if got := lastBytes(at, edgeview.LogTailBytes); !bytes.Equal(got, at) {
		t.Fatalf("at bound: got %d bytes, want %d", len(got), len(at))
	}
	over := append([]byte{'x'}, at...)
	got := lastBytes(over, edgeview.LogTailBytes)
	if len(got) != edgeview.LogTailBytes || got[0] != 'a' {
		t.Fatalf("over bound: len=%d first=%q", len(got), got[0])
	}
}

// TestProduceJobLogTailUsesLocalCache kills a mutation that publishes an
// empty tail despite a complete local copy and one that forgets the byte cap.
func TestProduceJobLogTailUsesLocalCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "run", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCompletionByID(database, jobID, 0, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil || job.LatestRunID == nil {
		t.Fatalf("completed job = %#v, err = %v", job, err)
	}
	content := append([]byte("discard"), bytes.Repeat([]byte{'z'}, edgeview.LogTailBytes)...)
	if err := logcache.WriteForRun(jobID, *job.LatestRunID, string(content)); err != nil {
		t.Fatal(err)
	}
	body, err := produceJobLogTailSection(context.Background(), edgeViewDeps{DB: database, Cfg: &config.Config{}}, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != edgeview.LogTailBytes || body[0] != 'z' {
		t.Fatalf("published tail: len=%d first=%q", len(body), body[0])
	}
}

// TestProduceQueuedJobLogTailPinsHubNotice kills a mutation that routes an
// unstarted rental through snapshot storage or publishes an empty section.
func TestProduceQueuedJobLogTailPinsHubNotice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "rental:17", "/tmp/project", "run", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		t.Fatalf("job = %#v, err = %v", job, err)
	}
	body, err := produceJobLogTailSection(context.Background(), edgeViewDeps{DB: database, Cfg: &config.Config{}}, jobID)
	if err != nil {
		t.Fatal(err)
	}
	want := queuedJobLogNotice(database, job)
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// TestRenderEdgeLogDefaultHintAndQueuedNotice kills mutations that omit the
// hub's default-tail hint or incorrectly add it to the queued-job notice.
func TestRenderEdgeLogDefaultHintAndQueuedNotice(t *testing.T) {
	setLogEdgeFlags(t, 50)
	transport, err := edge.NewFSTransport(filepath.Join(t.TempDir(), "view"))
	if err != nil {
		t.Fatal(err)
	}
	publisher := edgeview.NewPublisher(transport, "hub-a", "test")
	body := []byte("one\ntwo\n")
	if _, err := publisher.Publish(context.Background(), map[string]edgeview.Producer{
		edgeview.JobLogTailSection("wj42"): func(context.Context) ([]byte, error) { return body, nil },
	}); err != nil {
		t.Fatal(err)
	}
	em := &edgeMirrorRuntime{reader: edgeview.NewReader(transport), staleAfter: time.Minute}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := renderJobLogFromView(cmd, em, 42, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "(showing last 50 lines;") || !strings.Contains(out.String(), "one\ntwo\n") {
		t.Fatalf("normal output = %q", out.String())
	}

	queued := []byte("Job wj42 has not started yet; no logs are available.\nAttempts:    weft info wj42 --all-attempts\n")
	if _, err := publisher.Publish(context.Background(), map[string]edgeview.Producer{
		edgeview.JobLogTailSection("wj42"): func(context.Context) ([]byte, error) { return queued, nil },
	}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := renderJobLogFromView(cmd, em, 42, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "showing last 50 lines") || !strings.HasPrefix(out.String(), string(queued)) {
		t.Fatalf("queued output = %q", out.String())
	}
}

// TestValidateEdgeLogRangePinsCompleteLineThreshold kills a mutation from >
// to >= and one that silently serves a request beyond the bounded tail.
func TestValidateEdgeLogRangePinsCompleteLineThreshold(t *testing.T) {
	cmd := findCmd(t, "log")
	body := bytes.Repeat([]byte{'x'}, edgeview.LogTailBytes)
	copy(body[len(body)-6:], []byte("a\nb\nc\n"))
	linesFlag := cmd.Flags().Lookup("lines")
	savedChanged := linesFlag.Changed
	linesFlag.Changed = true
	t.Cleanup(func() { linesFlag.Changed = savedChanged })

	setLogEdgeFlags(t, 2)
	if err := validateEdgeLogRange(cmd, body); err != nil {
		t.Fatalf("two complete lines should fit: %v", err)
	}
	logLines = 3
	err := validateEdgeLogRange(cmd, body)
	const want = "requested --lines 3 exceeds the hub view log tail bound of 262144 bytes (2 complete lines available)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

// TestLogTailDeltaPinsAppendRollAndReplacement kills mutations that duplicate
// a rolling tail's overlap or suppress a replacement tail with no overlap.
func TestLogTailDeltaPinsAppendRollAndReplacement(t *testing.T) {
	for name, tc := range map[string]struct {
		previous string
		next     string
		want     string
	}{
		"append":      {previous: "one\n", next: "one\ntwo\n", want: "two\n"},
		"rolling":     {previous: "one\ntwo\n", next: "two\nthree\n", want: "three\n"},
		"replacement": {previous: "one\n", next: "other\n", want: "other\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := string(logTailDelta([]byte(tc.previous), []byte(tc.next))); got != tc.want {
				t.Fatalf("delta = %q, want %q", got, tc.want)
			}
		})
	}
}

type freshnessFlipWriter struct {
	bytes.Buffer
	stale *bool
}

func (w *freshnessFlipWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if strings.Contains(string(p), "source: hub") {
		*w.stale = true
	}
	return n, err
}

// TestLogFollowRechecksFreshness kills a mutation that checks freshness only
// on the initial tail read. The second cadence sees a stale section and blocks.
func TestLogFollowRechecksFreshness(t *testing.T) {
	transport, err := edge.NewFSTransport(filepath.Join(t.TempDir(), "view"))
	if err != nil {
		t.Fatal(err)
	}
	publisher := edgeview.NewPublisher(transport, "hub-a", "test")
	if _, err := publisher.Publish(context.Background(), map[string]edgeview.Producer{
		edgeview.JobLogTailSection("wj42"): func(context.Context) ([]byte, error) { return []byte("one\ntwo\n"), nil },
	}); err != nil {
		t.Fatal(err)
	}
	stale := false
	reader := edgeview.NewReader(transport).WithClock(func() time.Time {
		if stale {
			return time.Now().Add(10 * time.Minute)
		}
		return time.Now()
	})
	em := &edgeMirrorRuntime{reader: reader, staleAfter: 5 * time.Minute, publishInterval: time.Millisecond}
	setLogEdgeFlags(t, 50)
	logFollow = true
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stderr bytes.Buffer
	out := &freshnessFlipWriter{stale: &stale}
	cmd.SetOut(out)
	cmd.SetErr(&stderr)
	err = renderJobLogFromView(cmd, em, 42, true)
	var gateErr *EdgeGateError
	if !errors.As(err, &gateErr) || gateErr.ExitCode != edgeExitBlocked {
		t.Fatalf("error = %v, want blocked EdgeGateError", err)
	}
	if !strings.Contains(stderr.String(), "hub view for jobs/wj42/log.tail is 10m old") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
