package terminal

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/atotto/clipboard"
	"github.com/osteele/weft/internal/app/flash"
	"github.com/osteele/weft/internal/db"
)

// clipboardSeams swaps the clipboard seams for a test: native writes are
// captured instead of touching a real clipboard, and OSC 52 output goes to a
// buffer instead of a TTY. The environment is pinned local and
// multiplexer-free; tests that exercise those paths override it themselves.
type clipboardSeams struct {
	tty            *bytes.Buffer
	nativePayloads []string
	nativeErr      error
}

func swapClipboardSeams(t *testing.T, nativeErr error) *clipboardSeams {
	t.Helper()
	for _, key := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "TMUX"} {
		t.Setenv(key, "")
	}
	t.Setenv("TERM", "xterm-256color")
	seams := &clipboardSeams{tty: &bytes.Buffer{}, nativeErr: nativeErr}
	clipboardWriteFunc = func(s string) error {
		seams.nativePayloads = append(seams.nativePayloads, s)
		return seams.nativeErr
	}
	prev, _ := tuiTTYWriter.Load().(ttyWriterBox)
	tuiTTYWriter.Store(ttyWriterBox{w: seams.tty})
	t.Cleanup(func() {
		clipboardWriteFunc = clipboard.WriteAll
		tuiTTYWriter.Store(prev)
	})
	return seams
}

// runCopyCmd executes the copy command synchronously (tests do not run a tea
// program) and returns its message.
func runCopyCmd(t *testing.T, label, payload string) clipboardCopiedMsg {
	t.Helper()
	msg, ok := copyToClipboardCmd(label, payload)().(clipboardCopiedMsg)
	if !ok {
		t.Fatalf("copy command returned %T, want clipboardCopiedMsg", msg)
	}
	return msg
}

func TestCopyToClipboardNativeSuccess(t *testing.T) {
	seams := swapClipboardSeams(t, nil)

	msg := runCopyCmd(t, "blocking status", "blocked: no offers\nwj750")

	text, isError := msg.flashText()
	if text != "blocking status copied to clipboard" || isError {
		t.Fatalf("flash = %q, isError=%v; want %q, false", text, isError, "blocking status copied to clipboard")
	}
	if len(seams.nativePayloads) != 1 || seams.nativePayloads[0] != "blocked: no offers\nwj750" {
		t.Fatalf("native payloads = %q", seams.nativePayloads)
	}
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("blocked: no offers\nwj750")) + "\x07"
	if seams.tty.String() != want {
		t.Fatalf("tty = %q, want %q", seams.tty.String(), want)
	}
}

func TestCopyToClipboardNativeFailureFallsBackToOSC52(t *testing.T) {
	seams := swapClipboardSeams(t, errors.New("pbcopy: not found"))

	msg := runCopyCmd(t, "blocking status", "payload")

	text, isError := msg.flashText()
	want := "blocking status sent to terminal clipboard (local clipboard unavailable)"
	if text != want || isError {
		t.Fatalf("flash = %q, isError=%v; want %q, false", text, isError, want)
	}
	if len(seams.nativePayloads) != 1 {
		t.Fatalf("native payloads = %q, want one attempt", seams.nativePayloads)
	}
	if !strings.HasPrefix(seams.tty.String(), "\x1b]52;c;") {
		t.Fatalf("tty missing OSC 52 sequence: %q", seams.tty.String())
	}
}

func TestCopyToClipboardRemoteSessionSkipsNative(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	t.Setenv("SSH_CONNECTION", "10.0.0.2 51234 10.0.0.1 22")

	msg := runCopyCmd(t, "blocking status", "payload")

	text, isError := msg.flashText()
	want := "blocking status sent to terminal clipboard"
	if text != want || isError {
		t.Fatalf("flash = %q, isError=%v; want %q, false", text, isError, want)
	}
	if strings.Contains(text, "copied") {
		t.Fatalf("remote flash must say sent, not copied: %q", text)
	}
	if len(seams.nativePayloads) != 0 {
		t.Fatalf("remote session must not attempt native write, got %q", seams.nativePayloads)
	}
	if !strings.HasPrefix(seams.tty.String(), "\x1b]52;c;") {
		t.Fatalf("tty missing OSC 52 sequence: %q", seams.tty.String())
	}
}

func TestCopyToClipboardNoClipboardAvailable(t *testing.T) {
	seams := swapClipboardSeams(t, errors.New("pbcopy: not found"))
	tuiTTYWriter.Store(ttyWriterBox{}) // no TTY handle at all

	msg := runCopyCmd(t, "blocking status", "payload")

	text, isError := msg.flashText()
	if text != "copy failed: no clipboard available" || !isError {
		t.Fatalf("flash = %q, isError=%v; want %q, true", text, isError, "copy failed: no clipboard available")
	}
	if seams.tty.Len() != 0 {
		t.Fatalf("tty must stay empty, got %q", seams.tty.String())
	}
}

func TestCopyToClipboardOversizeRefused(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	payload := strings.Repeat("x", clipboardMaxPayload+1)

	msg := runCopyCmd(t, "blocking status", payload)

	text, isError := msg.flashText()
	want := "too large to copy (65537 bytes)"
	if text != want || !isError {
		t.Fatalf("flash = %q, isError=%v; want %q, true", text, isError, want)
	}
	if seams.tty.Len() != 0 {
		t.Fatalf("oversize payload must not emit a truncated clipboard, tty = %q", seams.tty.String())
	}
	if len(seams.nativePayloads) != 0 {
		t.Fatalf("oversize payload must not attempt native write, got %q", seams.nativePayloads)
	}

	// Exactly at the cap is still accepted.
	if msg := runCopyCmd(t, "blocking status", strings.Repeat("x", clipboardMaxPayload)); msg.tooLarge {
		t.Fatalf("payload at the cap must be accepted")
	}
}

func TestOSC52SequenceMultiplexerWrapping(t *testing.T) {
	payload := "hello"
	b64 := base64.StdEncoding.EncodeToString([]byte(payload))
	plain := "\x1b]52;c;" + b64 + "\x07"

	t.Run("plain", func(t *testing.T) {
		t.Setenv("TMUX", "")
		t.Setenv("TERM", "xterm-256color")
		if got := osc52Sequence(payload); got != plain {
			t.Fatalf("got %q, want %q", got, plain)
		}
	})
	t.Run("tmux", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
		t.Setenv("TERM", "xterm-256color")
		want := "\x1bPtmux;\x1b\x1b]52;c;" + b64 + "\x07\x1b\\"
		if got := osc52Sequence(payload); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("screen", func(t *testing.T) {
		t.Setenv("TMUX", "")
		t.Setenv("TERM", "screen-256color")
		want := "\x1bP\x1b]52;c;" + b64 + "\x07\x1b\\"
		if got := osc52Sequence(payload); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestWrapAndTruncateKeepsJobIDsOnContinuation(t *testing.T) {
	rows := []groupedStatusRow{{
		text:         "  blocked: " + strings.Repeat("a very long reason word ", 8),
		isBlocked:    true,
		wrap:         true,
		job:          &db.Job{ID: 750},
		launch:       &db.Launch{ID: 7001},
		expandToggle: "x",
		jobIDs:       []int64{750, 752},
	}}
	out := wrapAndTruncateGroupedStatusRows(rows, 40)
	if len(out) < 2 {
		t.Fatalf("expected wrapped continuation rows, got %d", len(out))
	}
	for i, row := range out[1:] {
		if len(row.jobIDs) != 2 || row.jobIDs[0] != 750 || row.jobIDs[1] != 752 {
			t.Fatalf("continuation %d lost jobIDs: %v", i, row.jobIDs)
		}
		if row.job != nil || row.launch != nil || row.expandToggle != "" {
			t.Fatalf("continuation %d kept selection-driving fields: %+v", i, row)
		}
	}
}

func TestBlockedBucketHeaderRowsCarrySortedJobIDs(t *testing.T) {
	reason := "run-rate headroom exhausted"
	jobs := []*db.Job{
		{ID: 760, Status: db.StatusQueued, QueueBlockedReason: reason, CreatedAt: 1},
		{ID: 752, Status: db.StatusQueued, QueueBlockedReason: reason, CreatedAt: 2},
		{ID: 750, Status: db.StatusQueued, QueueBlockedReason: reason, CreatedAt: 3},
		{ID: 751, Status: db.StatusQueued, QueueBlockedReason: reason, CreatedAt: 4},
	}
	rows := buildGroupedStatusRowsWithOptions(jobs, 0, groupedStatusRenderOptions{now: time.Unix(10_000, 0)})

	var header *groupedStatusRow
	for i := range rows {
		if strings.HasPrefix(rows[i].text, "  blocked: ") {
			header = &rows[i]
		}
	}
	if header == nil {
		t.Fatalf("no blocked bucket header row in %+v", rows)
	}
	want := []int64{750, 751, 752, 760}
	if len(header.jobIDs) != len(want) {
		t.Fatalf("header jobIDs = %v, want %v", header.jobIDs, want)
	}
	for i, id := range want {
		if header.jobIDs[i] != id {
			t.Fatalf("header jobIDs = %v, want %v", header.jobIDs, want)
		}
	}
}

func TestBlockedBucketCopyClickEndToEnd(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	reason := "run-rate headroom exhausted"
	jobs := []*db.Job{
		{ID: 750, Status: db.StatusQueued, QueueBlockedReason: reason, CreatedAt: 1},
		{ID: 751, Status: db.StatusQueued, QueueBlockedReason: reason, CreatedAt: 2},
		{ID: 752, Status: db.StatusQueued, QueueBlockedReason: reason, CreatedAt: 3},
		{ID: 760, Status: db.StatusQueued, QueueBlockedReason: reason, CreatedAt: 4},
	}
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           96,
		height:          30,
		jobs:            jobs,
	}
	m.rebuildGroupedRows()
	plan := m.buildGroupedScreenPlan()

	// Find the bucket header line on screen and click it.
	headerY := -1
	for y, line := range plan.Lines {
		if strings.Contains(stripANSI(line.Text), "blocked: "+reason) {
			headerY = y
			break
		}
	}
	if headerY < 0 {
		t.Fatalf("no blocked bucket header in frame:\n%s", plan.Render())
	}
	target, ok := plan.Hit(2, headerY)
	if !ok || target.kind != targetCopy || target.label != "blocking status" {
		t.Fatalf("hit = %+v, %v; want targetCopy labeled %q", target, ok, "blocking status")
	}

	cmd := m.dispatchTarget(target)
	if cmd == nil {
		t.Fatal("dispatchTarget returned no command")
	}
	msg, isCopy := cmd().(clipboardCopiedMsg)
	if !isCopy {
		t.Fatalf("copy command returned %T, want clipboardCopiedMsg", msg)
	}
	text, isError := msg.flashText()
	if text != "blocking status copied to clipboard" || isError {
		t.Fatalf("flash = %q, isError=%v", text, isError)
	}
	wantPayload := "blocked: " + reason + "\nwj750:wj752,wj760"
	if len(seams.nativePayloads) != 1 || seams.nativePayloads[0] != wantPayload {
		t.Fatalf("native payloads = %q, want [%q]", seams.nativePayloads, wantPayload)
	}
	if !strings.Contains(seams.tty.String(), base64.StdEncoding.EncodeToString([]byte(wantPayload))) {
		t.Fatalf("tty OSC 52 payload mismatch: %q", seams.tty.String())
	}
}

func TestIncidentRollupRowIsAJumpTarget(t *testing.T) {
	row := groupedStatusRow{
		text:                "  incident: no matching rental offers from providers — 2 jobs",
		isBlocked:           true,
		jobIDs:              []int64{750, 751},
		incidentFingerprint: "vastai/empty-result:no-offers",
		sampleJobID:         750,
	}
	target := groupedRowClickTarget(row, 3)
	if target.kind != targetIncidentJump || target.rowIdx != 3 {
		t.Fatalf("incident rollup row must jump to its sample job, got %+v", target)
	}
}

func TestFlashCoversPinnedQuickLaunchStatus(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		quickLaunching:  true,
		statusMessage:   "Launching new instance...",
		width:           96,
		height:          30,
	}
	if !m.quickLaunchStatusProtected() {
		t.Fatal("quick-launch status must be protected for this test")
	}

	next, _ := m.Update(clipboardCopiedMsg{label: "blocking status", nativeOK: true})
	m = next.(listTUIModel)
	if m.flash.Message != "blocking status copied to clipboard" {
		t.Fatalf("flash = %q", m.flash.Message)
	}
	if m.statusMessage != "Launching new instance..." {
		t.Fatalf("flash must not touch statusMessage, got %q", m.statusMessage)
	}
	if got := m.statusLineText(m.groupedStatusText()); !strings.Contains(got, "blocking status copied to clipboard") {
		t.Fatalf("status line must show the flash, got %q", got)
	}

	// When the flash expires, the pinned quick-launch status reappears intact.
	m.flash.Expiry = time.Now().Add(-time.Second)
	next, _ = m.Update(flash.ExpiredMsg{})
	m = next.(listTUIModel)
	if m.flash.Message != "" {
		t.Fatalf("flash must expire, got %q", m.flash.Message)
	}
	if got := m.statusLineText(m.groupedStatusText()); got != "Launching new instance..." {
		t.Fatalf("pinned status must reappear after flash expiry, got %q", got)
	}
}

func TestJobIDCopyFlashNamesTheJobID(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           96,
		height:          30,
		jobs:            []*db.Job{{ID: 504, Status: db.StatusRunning, CreatedAt: 1}},
	}
	m.rebuildGroupedRows()
	plan := m.buildGroupedScreenPlan()

	rowY := -1
	for y, line := range plan.Lines {
		if strings.Contains(stripANSI(line.Text), "wj504") {
			rowY = y
			break
		}
	}
	if rowY < 0 {
		t.Fatalf("no wj504 row in frame:\n%s", plan.Render())
	}

	target, ok := plan.Hit(2, rowY)
	if !ok || target.kind != targetCopy {
		t.Fatalf("hit = %+v, %v; want a copy target on the ID region", target, ok)
	}

	cmd := m.dispatchTarget(target)
	if cmd == nil {
		t.Fatal("dispatchTarget returned no command")
	}
	msg, isCopy := cmd().(clipboardCopiedMsg)
	if !isCopy {
		t.Fatalf("cmd produced %T, want clipboardCopiedMsg", cmd())
	}

	// The flash names the object copied, so the user can tell wj504 from wj505
	// without re-reading the row.
	text, isError := msg.flashText()
	if text != "wj504 copied to clipboard" || isError {
		t.Fatalf("flash = %q, isError=%v; want %q, false", text, isError, "wj504 copied to clipboard")
	}
	if len(seams.nativePayloads) != 1 || seams.nativePayloads[0] != "wj504" {
		t.Fatalf("clipboard payload = %q, want [wj504]", seams.nativePayloads)
	}
}
