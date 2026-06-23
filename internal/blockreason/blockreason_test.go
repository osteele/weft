package blockreason

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestResolveCompactBlockerSources(t *testing.T) {
	launchID := int64(3022)
	tests := []struct {
		name   string
		job    *db.Job
		opts   Options
		want   string
		kind   Kind
		source Source
	}{
		{
			name: "live queue reason wins",
			job: &db.Job{
				ID:                 1,
				Status:             db.StatusQueued,
				Host:               "cool30",
				QueueBlockedReason: "gpu gate: no GPU with 26GB free",
				PlacementReasons:   []string{"planner: no offers"},
			},
			want:   "gpu gate: no GPU with 26GB free",
			kind:   KindBlocked,
			source: SourceLive,
		},
		{
			name: "autopilot reason beats placement history",
			job: &db.Job{
				ID:               2,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"planner: older reason"},
			},
			opts:   Options{AutoPilotReason: "planner: current reason", Compact: true},
			want:   "planner: current reason",
			kind:   KindBlocked,
			source: SourceAutoPilot,
		},
		{
			name: "reuse-only autopilot diagnostic hidden",
			job: &db.Job{
				ID:     10,
				Status: db.StatusQueued,
			},
			opts: Options{
				AutoPilotReason: "could not reuse running instances: wi3816 RTX A6000 45GB: GPU memory insufficient: job=48GB",
			},
		},
		{
			name: "unplaced uses latest persisted placement reason",
			job: &db.Job{
				ID:               3,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"planner: older reason", "planner: no offers"},
			},
			want:   "planner: no offers",
			kind:   KindBlocked,
			source: SourcePlacement,
		},
		{
			name: "reuse-only placement history hidden",
			job: &db.Job{
				ID:               11,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"could not reuse running instances: wi3816 RTX A6000 45GB: GPU memory insufficient: job=48GB"},
			},
		},
		{
			name: "placement pending sentinel is mapped to waiting text",
			job: &db.Job{
				ID:               7,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"placement pending"},
			},
			want:   "autopilot",
			kind:   KindWaiting,
			source: SourcePlacement,
		},
		{
			name: "placement pending uses caller display text",
			job: &db.Job{
				ID:               12,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"placement pending"},
			},
			opts:   Options{PendingPlacementReason: "autopilot placing jobs"},
			want:   "autopilot placing jobs",
			kind:   KindWaiting,
			source: SourcePlacement,
		},
		{
			name: "placement pending delayed autopilot is waiting",
			job: &db.Job{
				ID:               13,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"placement pending"},
			},
			opts:   Options{PendingPlacementReason: "autopilot delayed 8m"},
			want:   "autopilot delayed 8m",
			kind:   KindWaiting,
			source: SourcePlacement,
		},
		{
			name: "inventory handoff is waiting",
			job: &db.Job{
				ID:               8,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"inventory-tagged: waiting for on-prem host"},
			},
			want:   "inventory-tagged: waiting for on-prem host",
			kind:   KindWaiting,
			source: SourcePlacement,
		},
		{
			name: "replan reset history is hidden",
			job: &db.Job{
				ID:               9,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"replan requested; previous rental placement canceled"},
			},
		},
		{
			name: "placed queued ignores stale reuse failure history",
			job: &db.Job{
				ID:       2031,
				Status:   db.StatusQueued,
				LaunchID: &launchID,
				PlacementReasons: []string{
					"reuse instance wi3022 failed: claim job wj2031 for instance wi3022: set launch_id for job wj2031: job 2031: job already claimed by another launch",
				},
			},
		},
		{
			name: "reset history is hidden",
			job: &db.Job{
				ID:               4,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"cloud instance 3025 failed (infra_failure)"},
			},
		},
		{
			name: "on-prem rejection hidden for rental eligible compact label",
			job: &db.Job{
				ID:               5,
				Status:           db.StatusQueued,
				PlacementReasons: []string{"3 hosts: host is opt-in only (specify with --host)"},
			},
		},
		{
			name: "on-prem rejection shown for inventory job",
			job: &db.Job{
				ID:               6,
				Status:           db.StatusQueued,
				Tags:             []string{db.TagInventory},
				PlacementReasons: []string{"3 hosts: host is opt-in only (specify with --host)"},
			},
			want:   "3 hosts: host is opt-in only (specify with --host)",
			kind:   KindBlocked,
			source: SourcePlacement,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Compact = true
			got := Resolve(tc.job, opts)
			if got.Reason != tc.want || got.Source != tc.source {
				t.Fatalf("Resolve() = (%q, %q), want (%q, %q)", got.Reason, got.Source, tc.want, tc.source)
			}
			if got.Kind != tc.kind {
				t.Fatalf("Kind = %q, want %q", got.Kind, tc.kind)
			}
			if got.Blocked != (tc.want != "") {
				t.Fatalf("Blocked = %v, want %v", got.Blocked, tc.want != "")
			}
		})
	}
}

func TestReasonsDetailFiltersHistory(t *testing.T) {
	job := &db.Job{
		ID:                 10,
		Status:             db.StatusQueued,
		QueueBlockedReason: `waiting for "output/model.pt" from wj1570 (running)`,
		PlacementReasons: []string{
			"cloud instance 3025 failed (infra_failure)",
			"no local host matched gpu-class=nvidia",
			"planner: no offers",
			"planner: no offers",
		},
	}
	reasons := Reasons(job, Options{CloudConfigured: true})
	got := strings.Join(reasons, "; ")
	want := `waiting for "output/model.pt" from wj1570 (running); planner: no offers`
	if got != want {
		t.Fatalf("Reasons() = %q, want %q", got, want)
	}
}

func TestReasonKindSourceSyncReasonsAreWaiting(t *testing.T) {
	for _, reason := range []string{
		"source sync already in flight",
		"[14m ago] source sync already in flight",
		"source sync already in flight: ~/code/research/project",
		"[21s ago, retry #2] source sync failed: rsync to cool30:~/code/research/project timed out",
		"source sync backing off (cool30 unresponsive), retry in 30s",
		"source sync deferred (host unreachable)",
	} {
		if got := ReasonKind(reason); got != KindWaiting {
			t.Fatalf("ReasonKind(%q) = %q, want %q", reason, got, KindWaiting)
		}
	}
}
