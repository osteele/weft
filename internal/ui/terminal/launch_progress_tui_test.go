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
