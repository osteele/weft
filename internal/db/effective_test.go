package db

import (
	"os"
	"testing"
)

func TestJobProject(t *testing.T) {
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
			job := &Job{WorkingDir: tt.workingDir, Command: tt.command}
			if got := job.Project(); got != tt.want {
				t.Errorf("Project() = %q, want %q", got, tt.want)
			}
		})
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
