package cmd

import (
	"fmt"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/placement"
	"github.com/spf13/cobra"
)

func TestPinnedPlatformFailsClosed(t *testing.T) {
	t.Cleanup(inventory.SetHosts([]inventory.HostSpec{{Name: "host-alpha", OS: "darwin", Arch: "arm64"}}))
	for _, host := range []string{"host-alpha", "host-not-in-inventory"} {
		if err := validatePinnedHostQueueGate(host, placement.Constraints{Platform: "linux/amd64"}); err == nil {
			t.Errorf("explicit host %s bypassed platform requirement", host)
		}
	}
}

func TestRestartPlatformParser(t *testing.T) {
	old, oldGPU, oldClass := restartPlatform, restartGPU, restartGPUClass
	oldScratch, oldCheckpoint := restartFromScratch, restartCheckpointed
	t.Cleanup(func() {
		restartPlatform, restartGPU, restartGPUClass = old, oldGPU, oldClass
		restartFromScratch, restartCheckpointed = oldScratch, oldCheckpoint
	})
	restartGPU, restartGPUClass = "", ""
	restartFromScratch, restartCheckpointed = false, false
	for _, tc := range []struct {
		raw, want string
		invalid   bool
	}{
		{"Linux/x86_64", "linux/amd64", false},
		{"", "", false},
		{"amd64", "", true},
	} {
		cmd := &cobra.Command{}
		cmd.Flags().StringVar(&restartPlatform, "platform", "", "")
		if err := cmd.Flags().Set("platform", tc.raw); err != nil {
			t.Fatal(err)
		}
		got, err := parseRestartOverrides(cmd)
		if tc.invalid {
			if err == nil {
				t.Fatal("invalid platform accepted")
			}
			continue
		}
		if err != nil || got.Platform == nil || *got.Platform != tc.want || !got.HasAny {
			t.Fatalf("parse %q: %+v, %v", tc.raw, got, err)
		}
	}
}

func TestPlatformRetryRejectsIncompatibleStoredPin(t *testing.T) {
	t.Cleanup(inventory.SetHosts([]inventory.HostSpec{{Name: "host-alpha", OS: "darwin", Arch: "arm64"}}))
	for _, action := range []string{"restart", "edit"} {
		t.Run(action, func(t *testing.T) {
			database := db.SetupTestDB(t)
			id, err := db.RecordQueued(database, "host-alpha", t.TempDir(), "echo numerical-work", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE jobs SET placement_host = 'host-alpha' WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE job_attempts SET status = ?, host = '', end_time = 1234, exit_code = 1 WHERE job_id = ?`, db.StatusFailed, id); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusFailed, id); err != nil {
				t.Fatal(err)
			}
			if err := db.SetJobCLIResourceOverrides(database, id, &db.CLIResourceOverrides{Platform: "linux/amd64"}); err != nil {
				t.Fatal(err)
			}
			before, err := db.GetJobByID(database, id)
			if err != nil {
				t.Fatal(err)
			}
			if before.Status != db.StatusFailed || before.LatestRunID == nil {
				t.Fatalf("fixture did not create a terminal historical attempt: %+v", before)
			}
			if action == "restart" {
				err = restartJob(database, id, restartOverrides{})
			} else {
				resetEditState()
				t.Cleanup(resetEditState)
				cmd := newEditTestCommand()
				if e := cmd.Flags().Set("retry", "true"); e != nil {
					t.Fatal(e)
				}
				err = runEdit(cmd, []string{fmt.Sprint(id)})
			}
			if err == nil {
				t.Fatal("retry admitted incompatible stored host")
			}
			after, err := db.GetJobByID(database, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != db.StatusFailed || after.RequestedPlatform() != "linux/amd64" || *after.LatestRunID != *before.LatestRunID {
				t.Fatalf("rejected retry changed execution: %+v", after)
			}
		})
	}
}

func TestRestartPlatformOverrideReleasesIncompatibleRental(t *testing.T) {
	for _, required := range []string{"linux/amd64", "darwin/arm64"} {
		t.Run(required, func(t *testing.T) {
			database := db.SetupTestDB(t)
			jobID, err := db.RecordQueued(database, "", t.TempDir(), "echo platform", "")
			if err != nil {
				t.Fatal(err)
			}
			launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = 1000, cloud_outcome = ? WHERE job_id = ?`,
				db.StatusFailed, db.AttemptOutcomeFailed, jobID); err != nil {
				t.Fatal(err)
			}
			if err := restartJob(database, jobID, restartOverrides{Platform: &required, HasAny: true}); err != nil {
				t.Fatal(err)
			}
			job, err := db.GetJobByID(database, jobID)
			if err != nil {
				t.Fatal(err)
			}
			if job.RequestedPlatform() != required {
				t.Fatalf("retry lost requested platform: %+v", job.CLIResourceOverrides)
			}
			if required == "linux/amd64" {
				if job.LaunchID == nil || *job.LaunchID != launchID {
					t.Fatalf("compatible rental was released: %+v", job)
				}
			} else if job.TargetKind() != db.JobTargetUnplaced {
				t.Fatalf("incompatible rental was retained: %+v", job)
			}
		})
	}
}
