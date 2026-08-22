package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/watchevents"
)

func TestFormatChannelStartupSummary(t *testing.T) {
	got := formatChannelStartupSummary("augur", 1, 2)
	for _, want := range []string{
		"augur has 3 unprocessed terminal jobs",
		"1 completed",
		"2 failed",
		"process-results skill",
		"weft jobs list --project augur --unprocessed --group-by status",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q: %q", want, got)
		}
	}

	empty := formatChannelStartupSummary("augur", 0, 0)
	if !strings.Contains(empty, "no unprocessed terminal jobs") {
		t.Fatalf("empty summary = %q", empty)
	}
}

func TestChannelStartupOmitsUnscopedInboxSummary(t *testing.T) {
	database := db.SetupTestDB(t)
	var out bytes.Buffer
	server := newChannelServer(database, "augur", "", 0, false, strings.NewReader(""), &out, &bytes.Buffer{})
	if err := server.emitStartupSummary(); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("unscoped startup emitted a reassuring inbox summary: %s", out.String())
	}
}

func TestChannelStartupSummaryPayload(t *testing.T) {
	database := db.SetupTestDB(t)
	const submitterSession = "session-channel-test"
	now := time.Now().Unix()
	exitZero := 0
	exitOne := 1

	completed, err := db.RecordQueued(database, "cool30", "/tmp/augur", "echo ok", "completed")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobProject(database, completed, "augur"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, completed, submitterSession); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseAttempt(database, completed, db.StatusCompleted, &exitZero, now); err != nil {
		t.Fatal(err)
	}

	failed, err := db.RecordQueued(database, "cool30", "/tmp/augur", "false", "failed")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobProject(database, failed, "augur"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, failed, submitterSession); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseAttempt(database, failed, db.StatusFailed, &exitOne, now); err != nil {
		t.Fatal(err)
	}

	processed, err := db.RecordQueued(database, "cool30", "/tmp/augur", "false", "processed")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobProject(database, processed, "augur"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, processed, submitterSession); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseAttempt(database, processed, db.StatusFailed, &exitOne, now); err != nil {
		t.Fatal(err)
	}
	if err := db.AddJobTag(database, processed, db.ProcessedTag); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	server := newChannelServer(database, "augur", submitterSession, 0, false, strings.NewReader(""), &out, &bytes.Buffer{})
	if err := server.emitStartupSummary(); err != nil {
		t.Fatal(err)
	}

	msg := decodeChannelMessage(t, out.Bytes())
	params := msg["params"].(map[string]any)
	content := params["content"].(string)
	if !strings.Contains(content, "1 completed, 1 failed") {
		t.Fatalf("content = %q", content)
	}
	meta := params["meta"].(map[string]any)
	if meta["event_type"] != "startup_summary" || meta["unprocessed_completed"] != "1" || meta["unprocessed_failed"] != "1" {
		t.Fatalf("bad meta: %#v", meta)
	}
}

func TestPrintChannelInstallJSON(t *testing.T) {
	var out bytes.Buffer
	if err := printChannelInstallJSON(&out, "weft", channelMCPServerEntry("weft")); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(out.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	weft := servers["weft"].(map[string]any)
	if weft["command"] != "weft" {
		t.Fatalf("command = %#v", weft["command"])
	}
	args := weft["args"].([]any)
	if len(args) != 2 || args[0] != "channel" || args[1] != "serve" {
		t.Fatalf("args = %#v", args)
	}
}

func TestChannelEventForJobUsesExistingIDs(t *testing.T) {
	exit := 1
	instanceID := int64(42)
	ev := watchevents.TransitionEvent{
		JobID:      "wj7",
		ID:         7,
		Status:     db.StatusFailed,
		Host:       "cool30",
		Project:    "augur",
		ExitCode:   &exit,
		InstanceID: &instanceID,
	}
	got := channelEventForJob(ev)
	for _, want := range []string{"augur/wj7 failed on cool30", "exit 1", "weft jobs logs wj7"} {
		if !strings.Contains(got.Content, want) {
			t.Fatalf("content missing %q: %q", want, got.Content)
		}
	}
	if got.Meta["job_id"] != "wj7" || got.Meta["cloud_instance_id"] != "wi42" || got.Meta["exit_code"] != "1" {
		t.Fatalf("bad meta: %#v", got.Meta)
	}
}

func TestChannelDebouncerCoalescesPerKey(t *testing.T) {
	gotCh := make(chan channelEvent, 2)
	d := newChannelDebouncer(10*time.Millisecond, func(ev channelEvent) {
		gotCh <- ev
	})
	defer d.Stop()

	d.Submit(channelDebounceKey{kind: "job", id: 1}, channelEvent{Content: "old"})
	d.Submit(channelDebounceKey{kind: "job", id: 1}, channelEvent{Content: "new"})
	d.Submit(channelDebounceKey{kind: "job", id: 2}, channelEvent{Content: "other"})

	got := make([]channelEvent, 0, 2)
	deadline := time.After(200 * time.Millisecond)
	for len(got) < 2 {
		select {
		case ev := <-gotCh:
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for debounced events; got %#v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d events: %#v", len(got), got)
	}
	contents := []string{got[0].Content, got[1].Content}
	joined := strings.Join(contents, ",")
	if strings.Contains(joined, "old") || !strings.Contains(joined, "new") || !strings.Contains(joined, "other") {
		t.Fatalf("unexpected events: %#v", contents)
	}
}

func decodeChannelMessage(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["method"] != "notifications/claude/channel" {
		t.Fatalf("method = %#v", msg["method"])
	}
	return msg
}
