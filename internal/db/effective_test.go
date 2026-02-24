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
