package terminal

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
)

// copyMouseTestModel builds a grouped-status model with one running job and a
// three-job blocked bucket, so a drag can span a section boundary and a
// bucket header.
func copyMouseTestModel(t *testing.T) listTUIModel {
	t.Helper()
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		mouseEnabled:    true,
		width:           96,
		height:          30,
		jobs: []*db.Job{
			{ID: 760, Status: db.StatusRunning, CreatedAt: 1},
			{ID: 750, Status: db.StatusQueued, QueueBlockedReason: "no offers", CreatedAt: 2},
			{ID: 751, Status: db.StatusQueued, QueueBlockedReason: "no offers", CreatedAt: 3},
			{ID: 752, Status: db.StatusQueued, QueueBlockedReason: "no offers", CreatedAt: 4},
		},
	}
	m.rebuildGroupedRows()
	return m
}

func mousePress(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: x, Y: y}
}

func mouseMotion(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion, X: x, Y: y}
}

func mouseRelease(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease, X: x, Y: y}
}

// sendMouse routes a mouse event through Update the way the running program
// does and returns the updated model.
func sendMouse(t *testing.T, m listTUIModel, msg tea.MouseMsg) listTUIModel {
	t.Helper()
	next, _ := m.Update(msg)
	nextList, ok := next.(listTUIModel)
	if !ok {
		t.Fatalf("Update returned %T, want listTUIModel", next)
	}
	return nextList
}

func sendKey(t *testing.T, m listTUIModel, key string) (listTUIModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(keyMsg(key))
	nextList, ok := next.(listTUIModel)
	if !ok {
		t.Fatalf("Update returned %T, want listTUIModel", next)
	}
	return nextList, cmd
}

// runCopyResult executes a copy command and returns its flash text.
func runCopyResult(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a copy command, got nil")
	}
	msg, ok := cmd().(clipboardCopiedMsg)
	if !ok {
		t.Fatalf("copy command returned %T, want clipboardCopiedMsg", cmd())
	}
	text, isError := msg.flashText()
	if isError {
		t.Fatalf("copy flash is an error: %q", text)
	}
	return text
}

func TestCopySelectionIDCopiesJobID(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		mouseEnabled:    true,
		width:           96,
		height:          30,
		jobs:            []*db.Job{{ID: 504, Status: db.StatusRunning, CreatedAt: 1}},
	}
	m.rebuildGroupedRows()

	_, cmd := sendKey(t, m, "y")

	if text := runCopyResult(t, cmd); text != "wj504 copied to clipboard" {
		t.Fatalf("flash = %q, want %q", text, "wj504 copied to clipboard")
	}
	if len(seams.nativePayloads) != 1 || seams.nativePayloads[0] != "wj504" {
		t.Fatalf("payloads = %q, want [wj504]", seams.nativePayloads)
	}
}

func TestCopySelectionIDCopiesInstanceID(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	launchID := int64(7001)
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		mouseEnabled:    true,
		width:           96,
		height:          30,
		jobs: []*db.Job{
			{ID: 510, Status: db.StatusPendingPlacement, LaunchID: &launchID, CreatedAt: 1},
			{ID: 511, Status: db.StatusPendingPlacement, LaunchID: &launchID, CreatedAt: 2},
		},
		launchByID: map[int64]*db.Launch{
			7001: {ID: 7001, Status: db.LaunchStatusLaunching, CreatedAt: 1},
		},
	}
	m.rebuildGroupedRows()

	headerIdx := -1
	for i, row := range m.groupedRows {
		if row.launch != nil {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		t.Fatal("no launching-instance header row")
	}
	m.selectGroupedRowByIndex(headerIdx)

	_, cmd := sendKey(t, m, "y")

	if text := runCopyResult(t, cmd); text != "wi7001 copied to clipboard" {
		t.Fatalf("flash = %q, want %q", text, "wi7001 copied to clipboard")
	}
	if len(seams.nativePayloads) != 1 || seams.nativePayloads[0] != "wi7001" {
		t.Fatalf("payloads = %q, want [wi7001]", seams.nativePayloads)
	}
}

func TestCopySelectionIDCopiesBlockedBucket(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		mouseEnabled:    true,
		width:           96,
		height:          30,
		groupedRows: []groupedStatusRow{
			{text: "  blocked: no offers (2)", isBlocked: true, jobIDs: []int64{750, 751}},
		},
		groupedSelectableRows: []int{0},
	}

	_, cmd := m.copySelectionID()

	if text := runCopyResult(t, cmd); text != "blocking status copied to clipboard" {
		t.Fatalf("flash = %q, want %q", text, "blocking status copied to clipboard")
	}
	want := "blocked: no offers\n" + ids.FormatJobIDListCompact([]int64{750, 751})
	if len(seams.nativePayloads) != 1 || seams.nativePayloads[0] != want {
		t.Fatalf("payloads = %q, want [%q]", seams.nativePayloads, want)
	}
}

func TestCopySelectionDetailsCopiesFooterBlock(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		mouseEnabled:    true,
		width:           96,
		height:          30,
		jobs:            []*db.Job{{ID: 511, Status: db.StatusRunning, Host: "cool30", CreatedAt: 1}},
	}
	m.rebuildGroupedRows()

	_, cmd := sendKey(t, m, "Y")

	if text := runCopyResult(t, cmd); text != "2 lines copied to clipboard" {
		t.Fatalf("flash = %q, want %q", text, "2 lines copied to clipboard")
	}
	if len(seams.nativePayloads) != 1 {
		t.Fatalf("payloads = %q, want one", seams.nativePayloads)
	}
	payload := seams.nativePayloads[0]
	if !strings.HasPrefix(payload, "Job: wj511") || !strings.Contains(payload, "\nHost: cool30") {
		t.Fatalf("payload = %q, want Job and Host lines", payload)
	}
}

func TestCopySelectionDetailsPluralizesOneLine(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		mouseEnabled:    true,
		width:           96,
		height:          30,
		jobs:            []*db.Job{{ID: 510, Status: db.StatusQueued, CreatedAt: 1}},
	}
	m.rebuildGroupedRows()

	_, cmd := sendKey(t, m, "Y")

	if text := runCopyResult(t, cmd); text != "1 line copied to clipboard" {
		t.Fatalf("flash = %q, want %q", text, "1 line copied to clipboard")
	}
	if len(seams.nativePayloads) != 1 || !strings.HasPrefix(seams.nativePayloads[0], "Job: wj510") {
		t.Fatalf("payloads = %q, want the Job line", seams.nativePayloads)
	}
	if strings.Contains(seams.nativePayloads[0], "\n") {
		t.Fatalf("single detail line must not contain a newline: %q", seams.nativePayloads[0])
	}
}

func TestRebalancePreviewYConfirmsNotCopies(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		mouseEnabled:    true,
		width:           96,
		height:          30,
		jobs:            []*db.Job{{ID: 504, Status: db.StatusRunning, CreatedAt: 1}},
	}
	m.rebuildGroupedRows()
	m.rebalancePreview = rebalancePreviewModel{
		active: true,
		moves:  []orchestration.QueueRebalanceMove{{JobID: 504, ToInstanceID: 7001}},
	}

	next, _ := m.Update(keyMsg("y"))
	m = next.(listTUIModel)

	if !m.rebalancePreview.applying {
		t.Fatal("y inside the rebalance preview must confirm the apply")
	}
	if m.statusMessage != "Applying rebalance moves…" {
		t.Fatalf("statusMessage = %q, want the apply confirmation", m.statusMessage)
	}
	if len(seams.nativePayloads) != 0 || seams.tty.Len() != 0 {
		t.Fatalf("y inside the preview must not copy: payloads=%q tty=%q", seams.nativePayloads, seams.tty.String())
	}
}

// planYForRowIdx returns the screen row of the plan line drawing model row
// rowIdx, or fails the test.
func planYForRowIdx(t *testing.T, plan screenPlan, rowIdx int) int {
	t.Helper()
	for y, line := range plan.lines {
		if line.rowIdx == rowIdx {
			return y
		}
	}
	t.Fatalf("no plan line draws row %d:\n%s", rowIdx, plan.render())
	return -1
}

// groupedRowIdxForJob returns the groupedRows index of the row drawing job id.
func groupedRowIdxForJob(t *testing.T, m listTUIModel, id int64) int {
	t.Helper()
	for i, row := range m.groupedRows {
		if row.job != nil && row.job.ID == id {
			return i
		}
	}
	t.Fatalf("no grouped row draws wj%d", id)
	return -1
}

// dragFromTo performs a press–motion–release drag between two model rows,
// re-resolving the motion target against a fresh plan the way a repaint
// between events would deliver it.
func dragFromTo(t *testing.T, m listTUIModel, fromRowIdx, toRowIdx int) listTUIModel {
	t.Helper()
	m = sendMouse(t, m, mousePress(40, planYForRowIdx(t, m.currentPlan(), fromRowIdx)))
	m = sendMouse(t, m, mouseMotion(40, planYForRowIdx(t, m.currentPlan(), toRowIdx)))
	return sendMouse(t, m, mouseRelease(40, 0))
}

func TestDragSelectsRowRange(t *testing.T) {
	useTrueColorProfile(t)
	m := copyMouseTestModel(t)

	from := groupedRowIdxForJob(t, m, 760)
	to := groupedRowIdxForJob(t, m, 750)
	m = dragFromTo(t, m, from, to)

	if !m.selRangeActive {
		t.Fatal("drag across rows must leave a range selection")
	}
	if job := m.selectedGroupedJob(); job == nil || job.ID != 750 {
		t.Fatalf("drag focus = %+v, want wj750", job)
	}
	// The span covers every job between the endpoints in display order,
	// whatever section order the rows render in.
	var wantIDs []int64
	for _, rowIdx := range m.groupedSelectableRows {
		if row := m.groupedRows[rowIdx]; row.job != nil {
			wantIDs = append(wantIDs, row.job.ID)
		}
	}
	if gotIDs := m.rangeSelectedJobIDs(); fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Fatalf("range job IDs = %v, want %v", gotIDs, wantIDs)
	}

	// Headers and the blocked-bucket line the drag crossed are spanned but
	// never highlighted.
	rangeRows := m.rangeSelectedRowIdxs()
	for i, row := range m.groupedRows {
		highlighted := rangeRows[i]
		if row.job != nil && !highlighted {
			t.Errorf("job row %d (wj%d) must be highlighted", i, row.job.ID)
		}
		if (row.isBlocked || row.isHeader) && highlighted {
			t.Errorf("header/status row %d (%q) must not be highlighted", i, row.text)
		}
	}

	plan := m.buildGroupedScreenPlan()
	headerY := planYForText(t, plan, "blocked: no offers")
	if strings.Contains(plan.lines[headerY].text, "\x1b[7m") {
		t.Error("bucket header line must not render selected")
	}
	for _, id := range []int64{750, 751} {
		y := planYForRowIdx(t, plan, groupedRowIdxForJob(t, m, id))
		if !strings.Contains(plan.lines[y].text, "\x1b[7m") {
			t.Errorf("wj%d line must render selected: %q", id, plan.lines[y].text)
		}
	}
}

func TestDragThenYCopiesCompactJobIDList(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	m := copyMouseTestModel(t)

	m = dragFromTo(t, m, groupedRowIdxForJob(t, m, 760), groupedRowIdxForJob(t, m, 750))

	_, cmd := sendKey(t, m, "y")

	if text := runCopyResult(t, cmd); text != "4 jobs copied to clipboard" {
		t.Fatalf("flash = %q, want %q", text, "4 jobs copied to clipboard")
	}
	want := ids.FormatJobIDListCompact([]int64{760, 750, 751, 752})
	if len(seams.nativePayloads) != 1 || seams.nativePayloads[0] != want {
		t.Fatalf("payloads = %q, want [%q]", seams.nativePayloads, want)
	}
}

func TestPlainClickSelectsSingleRow(t *testing.T) {
	m := copyMouseTestModel(t)

	y := planYForRowIdx(t, m.currentPlan(), groupedRowIdxForJob(t, m, 751))
	m = sendMouse(t, m, mousePress(40, y))
	m = sendMouse(t, m, mouseRelease(40, y))

	if m.selRangeActive {
		t.Fatal("a plain click must not leave a range selection")
	}
	if m.dragging {
		t.Fatal("release must end the drag")
	}
	if job := m.selectedGroupedJob(); job == nil || job.ID != 751 {
		t.Fatalf("click selection = %+v, want wj751", job)
	}
	if ids := m.rangeSelectedJobIDs(); ids != nil {
		t.Fatalf("plain click must not produce a job-ID range, got %v", ids)
	}
}

func TestWheelDoesNotStartDrag(t *testing.T) {
	m := copyMouseTestModel(t)
	cursor := m.cursor

	y := planYForRowIdx(t, m.currentPlan(), groupedRowIdxForJob(t, m, 751))
	wheel := tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress, X: 40, Y: y}
	next, cmd := m.Update(wheel)
	m = next.(listTUIModel)

	if cmd != nil {
		t.Fatal("wheel event must not dispatch a command")
	}
	if m.cursor != cursor || m.dragging || m.selRangeActive {
		t.Fatalf("wheel must not select or drag: cursor=%d dragging=%v range=%v", m.cursor, m.dragging, m.selRangeActive)
	}

	// Motion after a wheel event is not a drag either: no press started one.
	m = sendMouse(t, m, mouseMotion(40, y))
	if m.cursor != cursor || m.selRangeActive {
		t.Fatalf("motion without a press must not select a range: cursor=%d range=%v", m.cursor, m.selRangeActive)
	}
}

func TestFlatDragThenYCopiesCompactJobIDList(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	m := listTUIModel{
		title:        "Jobs",
		mouseEnabled: true,
		width:        96,
		height:       30,
		jobs: []*db.Job{
			{ID: 750, Status: db.StatusQueued, CreatedAt: 1},
			{ID: 751, Status: db.StatusQueued, CreatedAt: 2},
			{ID: 752, Status: db.StatusQueued, CreatedAt: 3},
		},
	}

	m = dragFromTo(t, m, 0, 2)

	if !m.selRangeActive || m.cursor != 2 {
		t.Fatalf("flat drag: cursor=%d range=%v, want cursor=2 with a range", m.cursor, m.selRangeActive)
	}

	_, cmd := sendKey(t, m, "y")

	if text := runCopyResult(t, cmd); text != "3 jobs copied to clipboard" {
		t.Fatalf("flash = %q, want %q", text, "3 jobs copied to clipboard")
	}
	want := ids.FormatJobIDListCompact([]int64{750, 751, 752})
	if len(seams.nativePayloads) != 1 || seams.nativePayloads[0] != want {
		t.Fatalf("payloads = %q, want [%q]", seams.nativePayloads, want)
	}
}

func TestMouseToggleKey(t *testing.T) {
	m := copyMouseTestModel(t)

	m, cmd := sendKey(t, m, "M")
	if m.mouseEnabled {
		t.Fatal("M must turn mouse reporting off")
	}
	if cmd == nil {
		t.Fatal("M must return a mouse-mode command")
	}
	if typ := fmt.Sprintf("%T", cmd()); typ != "tea.disableMouseMsg" {
		t.Fatalf("M-off command = %s, want tea.disableMouseMsg", typ)
	}
	if title := strings.SplitN(m.View(), "\n", 2)[0]; !strings.Contains(title, "mouse off (M to re-enable)") {
		t.Fatalf("title must show the mouse-off indicator, got %q", title)
	}

	m, cmd = sendKey(t, m, "M")
	if !m.mouseEnabled {
		t.Fatal("M must turn mouse reporting back on")
	}
	if typ := fmt.Sprintf("%T", cmd()); typ != "tea.enableMouseCellMotionMsg" {
		t.Fatalf("M-on command = %s, want tea.enableMouseCellMotionMsg", typ)
	}
	if title := strings.SplitN(m.View(), "\n", 2)[0]; strings.Contains(title, "mouse off") {
		t.Fatalf("title must drop the indicator once mouse is back on, got %q", title)
	}
}
