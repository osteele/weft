package db

import (
	"testing"
)

func TestEffectiveDescriptionFromDB(t *testing.T) {
	database, err := Open()
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	job, err := GetJobByID(database, 1159)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if job == nil {
		t.Skip("Job 1159 not found")
	}

	t.Logf("Job ID: %d", job.ID)
	t.Logf("Description: %q", job.Description)
	t.Logf("GeneratedDescription: %q", job.GeneratedDescription)
	t.Logf("GenerationHash: %q", job.GenerationHash)
	t.Logf("EffectiveDescription(): %q", job.EffectiveDescription())

	// Verify EffectiveDescription returns the generated description
	if job.Description == "" && job.GeneratedDescription != "" {
		if job.EffectiveDescription() != job.GeneratedDescription {
			t.Errorf("EffectiveDescription() = %q, want %q", job.EffectiveDescription(), job.GeneratedDescription)
		}
	}
}
