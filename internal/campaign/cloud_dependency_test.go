package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudneeds"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	weftsync "github.com/osteele/weft/internal/sync"
)

func dependencyAdmissionJobs(t *testing.T, database *sql.DB) (*db.Job, *db.Job) {
	t.Helper()
	producerID, err := db.RecordQueued(database, "", testProjectDir(t), "produce", "producer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAttempt(database, producerID, "", nil, db.StatusCompleted); err != nil {
		t.Fatal(err)
	}
	newJob := func(command string) *db.Job {
		id, err := db.RecordQueued(database, "", testProjectDir(t), command, command)
		if err != nil {
			t.Fatal(err)
		}
		job, err := db.GetJobByID(database, id)
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	consumer := newJob("consume")
	if err := db.SetJobNeeds(database, consumer.ID, []string{fmt.Sprintf("output/model:%d", producerID)}); err != nil {
		t.Fatal(err)
	}
	// Leave the caller's Needs stale: admission must read durable semantics.
	return consumer, newJob("healthy sibling")
}

func assertDependencyAdmissionState(t *testing.T, database *sql.DB, consumer, sibling *db.Job, failed bool) {
	t.Helper()
	got, err := db.GetJobByID(database, consumer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed {
		if got.Status != db.StatusFailed || got.FailureReason != db.FailureReasonDependencyFailed || !strings.Contains(got.ErrorMessage, "output/model") {
			t.Fatalf("consumer = status %q, failure %q, error %q; want actionable terminal dependency failure", got.Status, got.FailureReason, got.ErrorMessage)
		}
	} else if got.Status != db.StatusQueued || got.FailureReason != "" {
		t.Fatalf("consumer = status %q, failure %q; unconfirmed evidence must remain waitable", got.Status, got.FailureReason)
	}
	unplaced, err := db.ListUnplacedJobs(database)
	if err != nil {
		t.Fatal(err)
	}
	consumerEligible, siblingEligible := false, false
	for _, job := range unplaced {
		consumerEligible = consumerEligible || job.ID == consumer.ID
		siblingEligible = siblingEligible || job.ID == sibling.ID
	}
	if consumerEligible == failed || !siblingEligible {
		t.Fatalf("next-pass eligibility: consumer=%t sibling=%t, terminal=%t", consumerEligible, siblingEligible, failed)
	}
	metrics, err := queryRunawayMetrics(database, 0, []int64{consumer.ID, sibling.ID}, time.Now().Add(-time.Hour).Unix(), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if metrics.InfraFailureCount != 0 || metrics.OrphanedCount != 0 {
		t.Fatalf("dependency admission poisoned breaker counts: %+v", metrics)
	}
	providers, err := failingProvidersInWindow(database, []int64{consumer.ID, sibling.ID}, time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("dependency admission penalized healthy providers: %v", providers)
	}
}

func TestCloudDependencyPreflightBeforeAllocation(t *testing.T) {
	for _, admission := range []string{"fresh", "reuse"} {
		for _, tc := range []struct {
			name   string
			cause  error
			failed bool
		}{
			{"ready bytes absent", cloudneeds.ErrArtifactPublicationMissing, true},
			{"publication failed", cloudneeds.ErrArtifactPublicationFailed, true},
			{"publication pending", cloudneeds.ErrArtifactPublicationPending, false},
			{"publication unknown", cloudneeds.ErrArtifactPublicationUnknown, false},
			{"network unavailable", context.DeadlineExceeded, false},
		} {
			t.Run(admission+"/"+tc.name, func(t *testing.T) {
				database := db.SetupTestDB(t)
				consumer, sibling := dependencyAdmissionJobs(t, database)
				previous := resolveCloudNeedsFunc
				t.Cleanup(func() { resolveCloudNeedsFunc = previous })
				resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, specs []string) ([]cloud.CloudNeed, error) {
					return nil, fmt.Errorf("%w: requires %s", tc.cause, specs[0])
				}
				var err error
				if admission == "fresh" {
					client := &cloud.MockClient{ProviderVal: cloud.ProviderVastai, CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
						t.Fatal("provider creation must not occur after failed dependency admission")
						return nil, nil
					}}
					id, launchErr := LaunchInstance(client, nil, database, nil, InstanceGroup{Jobs: []*db.Job{sibling, consumer}}, cloud.Offer{Provider: cloud.ProviderVastai}, LaunchOpts{}, cloud.R2Config{}, cloud.CreateOpts{}, R2Assets{Client: &r2.Client{}}, nil, nil, nil)
					err = launchErr
					if id != 0 {
						t.Fatalf("allocated launch %d before dependency admission", id)
					}
				} else {
					id, createErr := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
					if createErr != nil {
						t.Fatal(createErr)
					}
					err = SubmitJobsToInstance(context.Background(), database, &r2.Client{}, id, []*db.Job{sibling, consumer})
					launch, readErr := db.GetLaunch(database, id)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if launch.Status != db.LaunchStatusRunning {
						t.Fatalf("healthy reuse launch changed to %s", launch.Status)
					}
				}
				if !errors.Is(err, tc.cause) {
					t.Fatalf("admission error = %v, want %v", err, tc.cause)
				}
				assertDependencyAdmissionState(t, database, consumer, sibling, tc.failed)
				var rentalAttempts int
				if err := database.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id IN (?, ?) AND launch_id IS NOT NULL`, consumer.ID, sibling.ID).Scan(&rentalAttempts); err != nil {
					t.Fatal(err)
				}
				if rentalAttempts != 0 {
					t.Fatalf("dependency admission created %d rental attempts", rentalAttempts)
				}
			})
		}
	}
}

func TestLaunchInstanceDependencyChangesAfterClaim(t *testing.T) {
	for _, cause := range []error{cloudneeds.ErrArtifactPublicationMissing, cloudneeds.ErrArtifactPublicationPending, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			database := db.SetupTestDB(t)
			consumer, sibling := dependencyAdmissionJobs(t, database)
			previous := resolveCloudNeedsFunc
			t.Cleanup(func() { resolveCloudNeedsFunc = previous })
			claimed := false
			resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, specs []string) ([]cloud.CloudNeed, error) {
				if claimed {
					return nil, fmt.Errorf("%w: requires %s", cause, specs[0])
				}
				return []cloud.CloudNeed{{Spec: specs[0], Path: "output/model", R2Key: "jobs/producer/output/model"}}, nil
			}
			client := &cloud.MockClient{ProviderVal: cloud.ProviderVastai, CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
				t.Fatal("provider create after rejected dependency")
				return nil, nil
			}}
			id, err := LaunchInstance(client, nil, database, nil, InstanceGroup{Jobs: []*db.Job{sibling, consumer}}, cloud.Offer{Provider: cloud.ProviderVastai}, LaunchOpts{}, cloud.R2Config{}, cloud.CreateOpts{}, R2Assets{Client: &r2.Client{}}, nil, nil, func(int64) { claimed = true })
			if !errors.Is(err, cause) || id == 0 {
				t.Fatalf("launch id=%d error=%v, want allocated launch rejected for %v", id, err, cause)
			}
			launch, err := db.GetLaunch(database, id)
			if err != nil {
				t.Fatal(err)
			}
			failed := confirmedCloudDependencyFailure(cause)
			wantReason := db.TerminationReasonCancelled
			if failed {
				wantReason = db.TerminationReasonInvalidRequest
			}
			if !db.IsTerminalLaunchStatus(launch.Status) || launch.TerminationReason != wantReason {
				t.Fatalf("launch status=%q reason=%q, want terminal %s", launch.Status, launch.TerminationReason, wantReason)
			}
			assertDependencyAdmissionState(t, database, consumer, sibling, failed)
		})
	}
}

func TestPreflightCloudNeedsDefersSameBatchProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	consumer, _ := dependencyAdmissionJobs(t, database)
	consumer, err := db.GetJobByID(database, consumer.ID)
	if err != nil {
		t.Fatal(err)
	}
	var producerID int64
	if _, err := fmt.Sscanf(consumer.Needs[0], "output/model:%d", &producerID); err != nil {
		t.Fatal(err)
	}
	producer, err := db.GetJobByID(database, producerID)
	if err != nil {
		t.Fatal(err)
	}
	previous := resolveCloudNeedsFunc
	t.Cleanup(func() { resolveCloudNeedsFunc = previous })
	resolveCloudNeedsFunc = func(context.Context, *sql.DB, *r2.Client, []string) ([]cloud.CloudNeed, error) {
		t.Fatal("same-batch dependency must be pinned after claims, not looked up in R2")
		return nil, nil
	}
	if err := preflightCloudNeeds(context.Background(), database, &r2.Client{}, []*db.Job{consumer, producer}, 0); err != nil {
		t.Fatal(err)
	}
}

func TestReuseDependencyChangesAfterClaim(t *testing.T) {
	for _, cause := range []error{cloudneeds.ErrArtifactPublicationMissing, cloudneeds.ErrArtifactPublicationPending, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			database := db.SetupTestDB(t)
			consumer, sibling := dependencyAdmissionJobs(t, database)
			instanceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090"})
			if err != nil {
				t.Fatal(err)
			}
			previousResolve := resolveCloudNeedsFunc
			previousUpload := uploadSourceToR2
			t.Cleanup(func() {
				resolveCloudNeedsFunc = previousResolve
				uploadSourceToR2 = previousUpload
			})
			claimed := false
			uploadSourceToR2 = func(_ context.Context, _ *r2.Client, sourceDir string, _ []string) (weftsync.SourceUploadResult, error) {
				claimed = true
				return testSourceUploadResult(sourceDir, "sources/test.tar.gz"), nil
			}
			resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, specs []string) ([]cloud.CloudNeed, error) {
				if claimed {
					return nil, fmt.Errorf("%w: requires %s", cause, specs[0])
				}
				return []cloud.CloudNeed{{Spec: specs[0], Path: "output/model", R2Key: "jobs/producer/output/model"}}, nil
			}
			err = SubmitJobsToInstance(context.Background(), database, nil, instanceID, []*db.Job{sibling, consumer})
			if !errors.Is(err, cause) || !claimed {
				t.Fatalf("claimed=%t error=%v, want late dependency rejection for %v", claimed, err, cause)
			}
			assertDependencyAdmissionState(t, database, consumer, sibling, confirmedCloudDependencyFailure(cause))
			launch, err := db.GetLaunch(database, instanceID)
			if err != nil {
				t.Fatal(err)
			}
			if launch.Status != db.LaunchStatusRunning {
				t.Fatalf("reuse failure changed healthy instance to %q", launch.Status)
			}
		})
	}
}
