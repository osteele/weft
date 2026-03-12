package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/cloudlog"
)

func TestFetchCloudLogLiveQueryAdjustment(t *testing.T) {
	manifest, _ := cloudlog.BuildChunks(99, []byte("1\n2\n3\n4\n5\n6\n"), 4)
	parts := cloudlog.SelectParts(manifest, 3, 5, 0)
	if len(parts) != 2 {
		t.Fatalf("selected %d parts, want 2", len(parts))
	}

	from, to, lines := cloudlog.AdjustQueryForParts(parts, 3, 5, 50)
	if from != 3 || to != 5 || lines != 50 {
		t.Fatalf("adjusted query = (%d,%d,%d), want (3,5,50)", from, to, lines)
	}
}
