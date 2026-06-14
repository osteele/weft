package db

import (
	"os"
	"testing"
)

func TestDeriveProject(t *testing.T) {
	tests := []struct {
		name       string
		workingDir string
		command    string
		want       string
	}{
		{"empty dir", "", "", ""},
		{"normal path", "/home/user/projects/my-project", "", "my-project"},
		{"trailing slash", "/home/user/projects/my-project/", "", "my-project"},
		{"tilde path", "~/projects/my-project", "", "my-project"},
		{"cd override", "", "cd ~/other-project && python train.py", "other-project"},
		{"dot path", ".", "", ""},
		{"root path", "/", "", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveProject(tt.workingDir, tt.command); got != tt.want {
				t.Errorf("DeriveProject(%q, %q) = %q, want %q", tt.workingDir, tt.command, got, tt.want)
			}
		})
	}
}

func TestNormalizeProjectName(t *testing.T) {
	tests := []struct {
		name       string
		project    string
		workingDir string
		command    string
		want       string
	}{
		{"explicit valid", "exp-123", "/tmp/project", "", "exp-123"},
		{"dot absolute dir", ".", "/tmp/adaptive-escalation", "", "adaptive-escalation"},
		{"empty uses dir", "", "/tmp/project", "", "project"},
		{"empty uses command cd", "", "", "cd /tmp/command-project && python train.py", "command-project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeProjectName(tt.project, tt.workingDir, tt.command)
			if err != nil {
				t.Fatalf("NormalizeProjectName: %v", err)
			}
			if got != tt.want {
				t.Errorf("NormalizeProjectName(%q, %q, %q) = %q, want %q", tt.project, tt.workingDir, tt.command, got, tt.want)
			}
		})
	}
}

func TestFilterJobsByProject(t *testing.T) {
	jobs := []*Job{
		{ID: 1, Project: "alpha"},
		{ID: 2, Project: "beta"},
		{ID: 3, Project: "alpha"},
		{ID: 4, Project: ""}, // no stored project
		{ID: 5, WorkingDir: "/home/user/gamma", Command: "echo hi"}, // derived from WorkingDir
	}

	// Filter by "alpha" returns matching stored projects
	filtered := FilterJobsByProject(jobs, "alpha")
	if len(filtered) != 2 {
		t.Errorf("FilterJobsByProject(alpha) got %d jobs, want 2", len(filtered))
	}

	// Empty filter returns all
	filtered = FilterJobsByProject(jobs, "")
	if len(filtered) != 5 {
		t.Errorf("FilterJobsByProject('') got %d jobs, want 5", len(filtered))
	}

	// Filter with fallback derivation
	filtered = FilterJobsByProject(jobs, "gamma")
	if len(filtered) != 1 || filtered[0].ID != 5 {
		t.Errorf("FilterJobsByProject(gamma) got %d jobs, want 1 (job 5)", len(filtered))
	}

	// Non-matching filter
	filtered = FilterJobsByProject(jobs, "nonexistent")
	if len(filtered) != 0 {
		t.Errorf("FilterJobsByProject(nonexistent) got %d jobs, want 0", len(filtered))
	}
}

func TestFilterJobsByIDSet(t *testing.T) {
	jobs := []*Job{{ID: 10}, {ID: 20}, {ID: 30}}

	got := FilterJobsByIDSet(jobs, map[int64]bool{10: true, 30: true})
	if len(got) != 2 || got[0].ID != 10 || got[1].ID != 30 {
		t.Errorf("FilterJobsByIDSet({10,30}) = %+v, want IDs [10, 30]", got)
	}

	if got := FilterJobsByIDSet(jobs, nil); len(got) != 3 {
		t.Errorf("nil filter dropped jobs: got %d, want 3", len(got))
	}
	if got := FilterJobsByIDSet(jobs, map[int64]bool{}); len(got) != 3 {
		t.Errorf("empty filter dropped jobs: got %d, want 3", len(got))
	}
}

func TestProjectHasAnyJobs(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "project-has-any-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	tmpfile.Close()
	defer os.Remove(tmpfile.Name())

	cleanup := SetDBPath(tmpfile.Name())
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	jobID, err := RecordJobStarting(database, "test-host", "/home/user/alpha", "echo hi", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := SetJobProject(database, jobID, "alpha"); err != nil {
		t.Fatalf("SetJobProject: %v", err)
	}

	has, err := ProjectHasAnyJobs(database, "alpha")
	if err != nil {
		t.Fatalf("ProjectHasAnyJobs(alpha): %v", err)
	}
	if !has {
		t.Errorf("ProjectHasAnyJobs(alpha) = false, want true")
	}

	has, err = ProjectHasAnyJobs(database, "nonexistent")
	if err != nil {
		t.Fatalf("ProjectHasAnyJobs(nonexistent): %v", err)
	}
	if has {
		t.Errorf("ProjectHasAnyJobs(nonexistent) = true, want false")
	}

	has, err = ProjectHasAnyJobs(database, "")
	if err != nil {
		t.Fatalf("ProjectHasAnyJobs(empty): %v", err)
	}
	if !has {
		t.Errorf("ProjectHasAnyJobs(\"\") = false, want true (empty means no narrowing)")
	}
}

func TestSetJobProjectRoundtrip(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "project-roundtrip-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	tmpfile.Close()
	defer os.Remove(tmpfile.Name())

	cleanup := SetDBPath(tmpfile.Name())
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	jobID, err := RecordJobStarting(database, "test-host", "/home/user/my-project", "echo hi", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	if err := SetJobProject(database, jobID, "my-project"); err != nil {
		t.Fatalf("SetJobProject: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Project != "my-project" {
		t.Errorf("job.Project = %q, want %q", job.Project, "my-project")
	}
}

func TestOpenRepairsPlaceholderProjects(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "project-repair-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	tmpfile.Close()
	defer os.Remove(tmpfile.Name())

	cleanup := SetDBPath(tmpfile.Name())
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	jobID, err := RecordQueuedWithGPU(database, "", "/Users/osteele/code/research/adaptive-escalation", "uv run python scripts/run_backtracking_search.py --resume", "retry", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := SetJobProject(database, jobID, "."); err != nil {
		t.Fatalf("set project: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET project = '.' WHERE id = ?`, jobID); err != nil {
		t.Fatalf("seed placeholder job project: %v", err)
	}
	defer database.Close()
	if err := startupRepair(database); err != nil {
		t.Fatalf("startup repair: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Project != "adaptive-escalation" {
		t.Fatalf("job.Project = %q, want %q", job.Project, "adaptive-escalation")
	}
}

func TestEffectiveDescriptionFromDB(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "effective-desc-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	tmpfile.Close()
	defer os.Remove(tmpfile.Name())

	cleanup := SetDBPath(tmpfile.Name())
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	jobID, err := RecordJobStarting(database, "test-host", "~/project", "cd ~/project && python train.py", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	// Generated description should be used when user description is empty
	if _, err := database.Exec(`UPDATE jobs SET description = '', generated_description = ?, generation_hash = ? WHERE id = ?`,
		"Generated summary", "hash1", jobID); err != nil {
		t.Fatalf("set generated description: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got := job.EffectiveDescription(); got != "Generated summary" {
		t.Fatalf("EffectiveDescription() = %q, want %q", got, "Generated summary")
	}

	// User description overrides generated description
	if _, err := database.Exec(`UPDATE jobs SET description = ?, generated_description = '' WHERE id = ?`,
		"User supplied", jobID); err != nil {
		t.Fatalf("set user description: %v", err)
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job second time: %v", err)
	}
	if got := job.EffectiveDescription(); got != "User supplied" {
		t.Fatalf("EffectiveDescription() = %q, want %q", got, "User supplied")
	}

	// Fallback to effective command when neither description present
	if _, err := database.Exec(`UPDATE jobs SET description = '', generated_description = '' WHERE id = ?`, jobID); err != nil {
		t.Fatalf("clear descriptions: %v", err)
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job third time: %v", err)
	}
	expectedCmd := "python train.py"
	if got := job.EffectiveDescription(); got != expectedCmd {
		t.Fatalf("EffectiveDescription() = %q, want %q", got, expectedCmd)
	}
}

func TestDirectoryTailDisplay(t *testing.T) {
	tests := []struct {
		name string
		job  Job
		want string
	}{
		{
			name: "uses effective working dir",
			job:  Job{WorkingDir: "~/code/alpha"},
			want: "alpha",
		},
		{
			name: "uses cd override from command",
			job:  Job{WorkingDir: "~/code/alpha", Command: "cd /tmp/beta && python train.py"},
			want: "beta",
		},
		{
			name: "falls back when unset",
			job:  Job{},
			want: "—",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.job.DirectoryTailDisplay(); got != tt.want {
				t.Fatalf("DirectoryTailDisplay() = %q, want %q", got, tt.want)
			}
		})
	}
}
