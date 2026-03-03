package llm

import (
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestGeneratorIntegration(t *testing.T) {
	if os.Getenv("LLM_TEST") == "" {
		t.Skip("set LLM_TEST=1 to run LLM integration tests")
	}

	client := NewDefaultClient()
	if !client.IsAvailable() {
		t.Skip("LLM backend not available")
	}

	database, err := db.Open()
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Create a generator and run one batch
	gen := NewGenerator(database, WithBatchSize(3))

	// Generate descriptions for a few jobs
	jobs, err := db.GetJobsNeedingDescriptions(database, 3)
	if err != nil {
		t.Fatalf("Failed to get jobs: %v", err)
	}

	if len(jobs) == 0 {
		t.Log("No jobs need descriptions")
		return
	}

	t.Logf("Found %d jobs needing descriptions", len(jobs))

	// Generate for first job
	job := jobs[0]
	desc, hash, err := gen.GenerateOne(job)
	if err != nil {
		t.Fatalf("Failed to generate description for job %d: %v", job.ID, err)
	}

	t.Logf("Generated for job %d: %s (hash: %s)", job.ID, desc, hash)

	// Update the database
	if err := db.UpdateJobGeneratedDescription(database, job.ID, desc, hash); err != nil {
		t.Fatalf("Failed to update job: %v", err)
	}

	// Verify it was saved
	updatedJob, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("Failed to get updated job: %v", err)
	}

	if updatedJob.GeneratedDescription != desc {
		t.Errorf("Description not saved correctly: got %q, want %q", updatedJob.GeneratedDescription, desc)
	}
	if updatedJob.GenerationHash != hash {
		t.Errorf("Hash not saved correctly: got %q, want %q", updatedJob.GenerationHash, hash)
	}

	t.Logf("Verified: job.EffectiveDescription() = %s", updatedJob.EffectiveDescription())
}

func TestBackgroundGenerator(t *testing.T) {
	if os.Getenv("LLM_TEST") == "" {
		t.Skip("set LLM_TEST=1 to run LLM integration tests")
	}

	client := NewDefaultClient()
	if !client.IsAvailable() {
		t.Skip("LLM backend not available")
	}

	database, err := db.Open()
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Count jobs needing descriptions before
	jobsBefore, _ := db.GetJobsNeedingDescriptions(database, 100)
	t.Logf("Jobs needing descriptions before: %d", len(jobsBefore))

	// Create and start generator
	gen := NewGenerator(database,
		WithBatchSize(5),
		WithInterval(1*time.Second),
	)

	if !gen.Start() {
		t.Fatal("Failed to start generator")
	}

	// Let it run for a few seconds
	time.Sleep(8 * time.Second)

	gen.Stop()

	// Count jobs needing descriptions after
	jobsAfter, _ := db.GetJobsNeedingDescriptions(database, 100)
	t.Logf("Jobs needing descriptions after: %d", len(jobsAfter))

	if len(jobsAfter) >= len(jobsBefore) && len(jobsBefore) > 0 {
		t.Error("Generator should have reduced the number of jobs needing descriptions")
	}
}
