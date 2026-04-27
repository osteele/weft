package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ui/terminal"
)

func TestFlattenProjectGroupJobsDedupesAcrossBuckets(t *testing.T) {
	j1 := &db.Job{ID: 1, Status: "running"}
	j2 := &db.Job{ID: 2, Status: "queued"}
	j3 := &db.Job{ID: 3, Status: "completed"}
	groups := []terminal.ProjectGroup{{
		Running:  []*db.Job{j1},
		Queued:   []*db.Job{j2, j2},
		Unplaced: []*db.Job{j2},
		Recent:   []*db.Job{j3, nil},
	}}
	jobs := flattenProjectGroupJobs(groups)
	seen := make(map[int64]bool)
	for _, j := range jobs {
		seen[j.ID] = true
	}
	if len(jobs) != 3 || !seen[1] || !seen[2] || !seen[3] {
		t.Fatalf("got %d jobs %v, want ids 1,2,3", len(jobs), seen)
	}
}

func TestProjectGroupsHaveActive(t *testing.T) {
	cases := []struct {
		name string
		g    terminal.ProjectGroup
		want bool
	}{
		{"running", terminal.ProjectGroup{Running: []*db.Job{{ID: 1}}}, true},
		{"queued", terminal.ProjectGroup{Queued: []*db.Job{{ID: 1}}}, true},
		{"unplaced", terminal.ProjectGroup{Unplaced: []*db.Job{{ID: 1}}}, true},
		{"recent-only", terminal.ProjectGroup{Recent: []*db.Job{{ID: 1}}}, false},
		{"empty", terminal.ProjectGroup{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectGroupsHaveActive([]terminal.ProjectGroup{tc.g}); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestProjectWatchRegistersEventFlags(t *testing.T) {
	for _, name := range []string{"transitions-only", "jsonl", "until-any-terminal", "follow"} {
		if projectWatchCmd.Flags().Lookup(name) == nil {
			t.Errorf("project watch missing flag --%s", name)
		}
	}
}

func TestWatchPlainOptionsFromGlobals(t *testing.T) {
	prevFollow, prevTrans, prevJSON, prevUntil := watchFollow, watchTransitionsOnly, watchJSONLines, watchUntilAnyTerminal
	t.Cleanup(func() {
		watchFollow, watchTransitionsOnly, watchJSONLines, watchUntilAnyTerminal = prevFollow, prevTrans, prevJSON, prevUntil
	})
	watchFollow = true
	watchTransitionsOnly = true
	watchJSONLines = true
	watchUntilAnyTerminal = false
	opts := watchPlainOptions()
	if !opts.Follow || !opts.TransitionsOnly || !opts.JSONLines || opts.UntilAnyTerminal {
		t.Errorf("unexpected options: %+v", opts)
	}
}
