package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectExtraPaths_RelativeResolution(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	localDir := filepath.Join(home, "code", "research", "structural-probes")

	tests := []struct {
		name     string
		inputs   []string
		localDir string
		want     []string
	}{
		{
			name:     "relative path resolved to tilde-relative",
			inputs:   []string{"local:data/"},
			localDir: localDir,
			want:     []string{"~/code/research/structural-probes/data"},
		},
		{
			name:     "tilde path passed through",
			inputs:   []string{"~/external/data/"},
			localDir: localDir,
			want:     []string{"~/external/data/"},
		},
		{
			name:     "absolute path passed through",
			inputs:   []string{"/tmp/data"},
			localDir: localDir,
			want:     []string{"/tmp/data"},
		},
		{
			name:     "asset refs excluded",
			inputs:   []string{"hf:gpt2", "local:cache/"},
			localDir: localDir,
			want:     []string{"~/code/research/structural-probes/cache"},
		},
		{
			name:     "mixed inputs",
			inputs:   []string{"local:data/", "hf:gpt2", "~/shared/models/"},
			localDir: localDir,
			want:     []string{"~/code/research/structural-probes/data", "~/shared/models/"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CollectExtraPaths(tt.inputs, tt.localDir)
			if len(got) != len(tt.want) {
				t.Fatalf("CollectExtraPaths() returned %d paths, want %d: %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("path[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
