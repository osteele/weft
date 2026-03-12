package cloudlog

import (
	"testing"
)

func TestBuildChunks_SplitsAtNewlinesAndTracksLineRanges(t *testing.T) {
	content := []byte("aaa\nbbb\nccc\nddd\n")
	manifest, chunks := BuildChunks(42, content, 7)

	if len(chunks) != 2 {
		t.Fatalf("chunk count = %d, want 2", len(chunks))
	}
	if string(chunks[0].Content) != "aaa\nbbb\n" {
		t.Fatalf("chunk 1 content = %q", string(chunks[0].Content))
	}
	if string(chunks[1].Content) != "ccc\nddd\n" {
		t.Fatalf("chunk 2 content = %q", string(chunks[1].Content))
	}
	if manifest.TotalLines != 4 {
		t.Fatalf("total lines = %d, want 4", manifest.TotalLines)
	}
	if chunks[0].Part.StartLine != 1 || chunks[0].Part.EndLine != 2 || !chunks[0].Part.Sealed {
		t.Fatalf("chunk 1 metadata = %+v", chunks[0].Part)
	}
	if chunks[1].Part.StartLine != 3 || chunks[1].Part.EndLine != 4 || chunks[1].Part.Sealed {
		t.Fatalf("chunk 2 metadata = %+v", chunks[1].Part)
	}
}

func TestSelectParts_ForTailAndRange(t *testing.T) {
	content := []byte("1\n2\n3\n4\n5\n")
	manifest, _ := BuildChunks(42, content, 4)

	tail := SelectParts(manifest, 0, 0, 2)
	if len(tail) != 1 || tail[0].StartLine != 4 || tail[0].EndLine != 5 {
		t.Fatalf("tail selection = %+v", tail)
	}

	rng := SelectParts(manifest, 2, 4, 0)
	if len(rng) != 2 {
		t.Fatalf("range selection count = %d, want 2", len(rng))
	}
	if rng[0].StartLine > 2 || rng[len(rng)-1].EndLine < 4 {
		t.Fatalf("range selection = %+v", rng)
	}
}

func TestAdjustQueryForParts(t *testing.T) {
	parts := []Part{
		{StartLine: 11, EndLine: 20},
		{StartLine: 21, EndLine: 30},
	}

	from, to, lines := AdjustQueryForParts(parts, 15, 22, 50)
	if from != 5 || to != 12 || lines != 50 {
		t.Fatalf("adjusted query = (%d,%d,%d), want (5,12,50)", from, to, lines)
	}
}

func TestBuildRunChunks_UsesRunScopedKeys(t *testing.T) {
	manifest, chunks := BuildRunChunks(42, 7, []byte("aaa\nbbb\n"), 4)

	if len(chunks) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(chunks))
	}
	wantKey := "jobs/42/runs/7/live-log/part-000001.log"
	if chunks[0].Part.Key != wantKey {
		t.Fatalf("chunk key = %q, want %q", chunks[0].Part.Key, wantKey)
	}
	if manifest.Parts[0].Key != wantKey {
		t.Fatalf("manifest part key = %q, want %q", manifest.Parts[0].Key, wantKey)
	}
}
