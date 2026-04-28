package terminal

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func queuedJob(id int64, reason string) *db.Job {
	return &db.Job{ID: id, Host: "cool30", Status: db.StatusQueued, QueueBlockedReason: reason}
}

func runningJob(id int64) *db.Job {
	return &db.Job{ID: id, Host: "cool30", Status: db.StatusRunning}
}

func TestSummarizeOnPremHostBlock(t *testing.T) {
	cases := []struct {
		name string
		jobs []*db.Job
		want string
	}{
		{
			name: "no jobs",
			jobs: nil,
			want: "",
		},
		{
			name: "running job suppresses summary",
			jobs: []*db.Job{runningJob(1), queuedJob(2, "gpu gate: foo")},
			want: "",
		},
		{
			name: "no reasons recorded",
			jobs: []*db.Job{queuedJob(1, "")},
			want: "",
		},
		{
			name: "single queued job blocked",
			jobs: []*db.Job{queuedJob(1, "gpu gate: no GPU with 26GB free")},
			want: "blocked: gpu gate: no GPU with 26GB free",
		},
		{
			name: "all queued share one reason",
			jobs: []*db.Job{
				queuedJob(1, "gpu gate: no GPU with 26GB free"),
				queuedJob(2, "gpu gate: no GPU with 26GB free"),
				queuedJob(3, "gpu gate: no GPU with 26GB free"),
			},
			want: "all 3 queued blocked: gpu gate: no GPU with 26GB free",
		},
		{
			name: "majority blocked, one missing reason",
			jobs: []*db.Job{
				queuedJob(1, "gpu gate: no GPU with 26GB free"),
				queuedJob(2, "gpu gate: no GPU with 26GB free"),
				queuedJob(3, ""),
			},
			want: "2/3 blocked: gpu gate: no GPU with 26GB free",
		},
		{
			name: "split reasons",
			jobs: []*db.Job{
				queuedJob(1, "gpu gate: A"),
				queuedJob(2, "gpu gate: A"),
				queuedJob(3, "cpu gate: B"),
			},
			want: "2/3 blocked: gpu gate: A; +1 other",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeOnPremHostBlock(onPremHostSummary{Name: "cool30", Jobs: tc.jobs})
			if got != tc.want {
				t.Errorf("summarizeOnPremHostBlock = %q, want %q", got, tc.want)
			}
		})
	}
}
