package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

func TestFormatMoveExitSummaryAt_PrintsReceipt(t *testing.T) {
	now := time.Unix(1778230000, 0)
	out := FormatMoveExitSummaryAt(nil, []*db.Job{{ID: 1862}, {ID: 1863}}, []int64{2672}, now, 96)

	for _, want := range []string{
		"weft move ended - ",
		"Moved: wj1862,wj1863 -> 1 new instance",
		"Instances:",
		"wi2672",
		"Next: weft watch instance",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("summary should be plain text, got ANSI:\n%q", out)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if lipgloss.Width(line) > 96 {
			t.Fatalf("line width = %d, want <= 96: %q", lipgloss.Width(line), line)
		}
	}
}
