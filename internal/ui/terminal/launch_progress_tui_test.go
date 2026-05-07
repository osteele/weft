package terminal

import (
	"strings"
	"testing"
	"time"
)

func TestLaunchProgressView_OmitsCampaignNumberAndDuplicateElapsed(t *testing.T) {
	m := launchProgressModel{
		startedAt:       time.Now().Add(-24 * time.Second),
		campaignID:      355,
		expectedWorkers: 1,
		headerMsg:       "launching worker instances",
	}

	out := m.View()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(firstLine, "Launching 1 instance · ") {
		t.Fatalf("header = %q, want singular launch count", firstLine)
	}
	if strings.Contains(out, "Campaign 355") {
		t.Fatalf("output includes campaign number:\n%s", out)
	}
	if strings.Contains(out, "launching worker instances") {
		t.Fatalf("output includes generic launch status:\n%s", out)
	}
	if strings.Count(out, "elapsed") != 1 {
		t.Fatalf("output should include elapsed once, got:\n%s", out)
	}
	if strings.Contains(out, "ready 0/1 · failed 0 · elapsed") {
		t.Fatalf("footer repeats elapsed:\n%s", out)
	}
}

func TestLaunchProgressHeaderMessageKeepsSpecificStatus(t *testing.T) {
	if got := launchProgressHeaderMessage("preparing launch plan"); got != "preparing launch plan" {
		t.Fatalf("launchProgressHeaderMessage returned %q", got)
	}
}

func TestLaunchProgressRowPhaseShowsElapsed(t *testing.T) {
	now := time.Unix(1000, 0)
	row := &launchProgressRow{
		phase:          "waiting for SSH",
		phaseStartedAt: now.Add(-2*time.Minute - 3*time.Second),
	}

	if got := launchProgressRowPhase(row, now); got != "waiting for SSH 02:03" {
		t.Fatalf("launchProgressRowPhase = %q", got)
	}
}

func TestLaunchProgressRowPhaseDoesNotChangeCompletedRows(t *testing.T) {
	now := time.Unix(1000, 0)
	row := &launchProgressRow{
		phase:          "running bootstrap script",
		phaseStartedAt: now.Add(-2 * time.Minute),
		done:           true,
	}

	if got := launchProgressRowPhase(row, now); got != "running bootstrap script" {
		t.Fatalf("launchProgressRowPhase = %q", got)
	}
}

func TestLaunchProgressCompleteMarksPendingRowsDone(t *testing.T) {
	now := time.Unix(1000, 0)
	m := launchProgressModel{
		startedAt:       now.Add(-2 * time.Minute),
		expectedWorkers: 1,
		width:           100,
		rowOrder:        []string{"g1"},
		rows: map[string]*launchProgressRow{
			"g1": {
				jobLabel:       "wj1769",
				constraint:     "NVIDIA >=42GB <=48GB",
				phase:          "uploading bootstrap",
				phaseStartedAt: now.Add(-time.Second),
				assetsReady:    2,
				assetsTotal:    2,
			},
		},
	}

	m.markSuccessfulRowsDone([]int64{2549})
	row := m.rows["g1"]
	if !row.done {
		t.Fatal("row.done = false, want true")
	}
	if row.instanceID != 2549 {
		t.Fatalf("row.instanceID = %d, want 2549", row.instanceID)
	}
	out := stripANSI(m.renderRow(row, now))
	if !strings.Contains(out, "wi2549 ready") {
		t.Fatalf("completed row did not render final ready state: %q", out)
	}
	if strings.Contains(out, "uploading bootstrap") {
		t.Fatalf("completed row still renders transient phase: %q", out)
	}
	if strings.Contains(out, "assets 2/2") {
		t.Fatalf("completed row still renders asset progress: %q", out)
	}
}

func TestLaunchProgressRenderRowFitsNarrowTerminal(t *testing.T) {
	now := time.Unix(1000, 0)
	m := launchProgressModel{width: 80}
	row := &launchProgressRow{
		jobLabel:       "wj1777",
		constraint:     "NVIDIA >=42GB <=48GB",
		phase:          "uploading bootstrap",
		phaseStartedAt: now,
		assetsReady:    2,
		assetsTotal:    2,
		retryAttempt:   2,
		retryMax:       4,
	}

	out := stripANSI(m.renderRow(row, now))
	if got := displayWidth(out); got > 80 {
		t.Fatalf("row width = %d, want <= 80; row=%q", got, out)
	}
	if strings.Contains(out, "assets 2/2") {
		t.Fatalf("narrow row should omit asset progress before wrapping: %q", out)
	}
	if !strings.Contains(out, "retry 2/4") {
		t.Fatalf("narrow row should retain retry badge: %q", out)
	}
}
