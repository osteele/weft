package campaign

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

// setupTestDB creates an in-memory SQLite database for testing.
func setupTestDB(t *testing.T) *sql.DB {
	return db.SetupTestDB(t)
}

func TestLaunchInstanceNilR2Client(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	var createdOfferID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			createdOfferID = offerID
			return &cloud.Instance{
				ProviderID:  "12345",
				Status:      "running",
				SSHHost:     "127.0.0.1",
				SSHPort:     19999,
				CostPerHour: 0.50,
			}, nil
		},
		DestroyInstanceFunc: func(instanceID string) error {
			return nil
		},
	}

	job := &db.Job{
		ID:      101,
		Status:  db.StatusQueued,
		Command: "python train.py",
	}

	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs:     []*db.Job{job},
	}

	offer := cloud.Offer{
		ProviderID:  "999",
		Provider:    cloud.ProviderVastai,
		GPUName:     "RTX_4090",
		NumGPUs:     1,
		GPUMemGB:    24,
		CostPerHour: 0.50,
	}

	r2Cfg := cloud.R2Config{
		AccountID:       "test-account",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		Bucket:          "test-bucket",
	}

	createOpts := cloud.CreateOpts{
		Image:      "nvidia/cuda:12.4.1-runtime-ubuntu22.04",
		DiskGB:     50,
		SSHEnabled: true,
	}

	_, err := LaunchInstance(
		mockClient, database, nil, group, offer,
		LaunchOpts{},
		r2Cfg, createOpts,
		R2Assets{Client: nil, AgentR2Key: "agents/test-version/linux-amd64", SourceR2Keys: map[string]string{}},
		nil,
		func(phase string) {},
		nil,
	)
	if err == nil {
		t.Fatal("expected error for nil r2Client, got nil")
	}
	if !strings.Contains(err.Error(), "R2Assets.Client is required") {
		t.Errorf("error should mention R2Assets.Client requirement, got: %v", err)
	}

	// Verify CreateInstance was NOT called (we fail before that)
	if createdOfferID != "" {
		t.Errorf("expected no CreateInstance call, got offer ID %q", createdOfferID)
	}
}

func TestLaunchInstanceCreateFails(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, errors.New("API error: insufficient balance")
		},
	}

	job := &db.Job{
		ID:      101,
		Status:  db.StatusQueued,
		Command: "python train.py",
	}

	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs:     []*db.Job{job},
	}

	offer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai}
	r2Cfg := cloud.R2Config{Bucket: "test", AccountID: "test"}
	createOpts := cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, '/tmp', ?, ?, ?, 0)`,
		job.ID, group.GPUClass, group.GPUMemGB, job.Command,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	instanceID, err := LaunchInstance(
		mockClient, database, nil, group, offer,
		LaunchOpts{},
		r2Cfg, createOpts,
		R2Assets{Client: &r2.Client{}},
		nil,
		func(phase string) {},
		nil,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error: insufficient balance") {
		t.Errorf("error should include create-instance failure, got: %v", err)
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get cloud instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
	if ci.TerminationReason != db.TerminationReasonInfraFailure {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, db.TerminationReasonInfraFailure)
	}

	// After failed launch, the job should be reset to unplaced (via job_status view)
	resetJob, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if resetJob.LaunchID != nil {
		t.Fatalf("job cloud_instance_id = %v, want nil", *resetJob.LaunchID)
	}

	attempts, err := db.GetLaunchAttempts(database, job.ID)
	if err != nil {
		t.Fatalf("get job attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d, want 1", len(attempts))
	}
	if attempts[0].Outcome != db.AttemptOutcomeOrphaned {
		t.Fatalf("attempt outcome = %q, want %q", attempts[0].Outcome, db.AttemptOutcomeOrphaned)
	}
	if attempts[0].EndedAt == nil {
		t.Fatal("attempt ended_at = nil, want non-nil")
	}
}

func TestLaunchInstanceRegistersInstanceBeforeProviderCreateCompletes(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, errors.New("stop after registration")
		},
	}

	job := &db.Job{
		ID:      101,
		Status:  db.StatusQueued,
		Command: "python train.py",
	}
	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs:     []*db.Job{job},
	}
	offer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai}
	r2Cfg := cloud.R2Config{Bucket: "test", AccountID: "test"}
	createOpts := cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, '/tmp', ?, ?, ?, 0)`,
		job.ID, group.GPUClass, group.GPUMemGB, job.Command,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	var callbackInstanceID int64
	var callbackJobLaunch sql.NullInt64
	var callbackJobHost string
	instanceID, err := LaunchInstance(
		mockClient, database, nil, group, offer,
		LaunchOpts{},
		r2Cfg, createOpts,
		R2Assets{Client: &r2.Client{}},
		nil,
		func(string) {},
		func(registeredID int64) {
			callbackInstanceID = registeredID

			ci, err := db.GetLaunch(database, registeredID)
			if err != nil {
				t.Fatalf("get registered cloud instance: %v", err)
			}
			if ci.Status != db.LaunchStatusLaunching {
				t.Fatalf("registered instance status = %q, want %q", ci.Status, db.LaunchStatusLaunching)
			}

			if err := database.QueryRow(`SELECT launch_id, host FROM job_status WHERE id = ?`, job.ID).Scan(&callbackJobLaunch, &callbackJobHost); err != nil {
				t.Fatalf("select job during registration callback: %v", err)
			}
		},
	)
	if err == nil || !strings.Contains(err.Error(), "stop after registration") {
		t.Fatalf("LaunchInstance() error = %v, want stop-after-registration failure", err)
	}
	if instanceID == 0 {
		t.Fatal("expected DB instance ID")
	}
	if callbackInstanceID != instanceID {
		t.Fatalf("callback instance ID = %d, want %d", callbackInstanceID, instanceID)
	}
	if !callbackJobLaunch.Valid || callbackJobLaunch.Int64 != instanceID {
		t.Fatalf("callback job cloud_instance_id = %+v, want %d", callbackJobLaunch, instanceID)
	}
	if callbackJobHost != "" {
		t.Fatalf("callback job host = %q, want empty string", callbackJobHost)
	}
}

func TestLaunchCampaignRejectsEmptyGroups(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	result, err := LaunchCampaign(
		nil, database, nil, nil, nil, nil, LaunchOpts{}, cloud.R2Config{}, nil,
		nil, nil, nil,
	)
	if err == nil {
		t.Fatal("expected error for empty groups, got nil")
	}
	if result != nil {
		t.Fatalf("expected nil result on error, got %#v", result)
	}
	if !strings.Contains(err.Error(), "no instance groups to launch") {
		t.Fatalf("error = %v, want message about empty launch groups", err)
	}

	var campaigns int
	if scanErr := database.QueryRow(`SELECT COUNT(*) FROM campaigns`).Scan(&campaigns); scanErr != nil {
		t.Fatalf("count campaigns: %v", scanErr)
	}
	if campaigns != 0 {
		t.Fatalf("campaign count = %d, want 0", campaigns)
	}
}

func TestCreateInstanceWithReplacementRetriesUnavailableOffer(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24}
	initialOffer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai, CostPerHour: 1.00}
	replacement := cloud.Offer{ProviderID: "1001", Provider: cloud.ProviderVastai, CostPerHour: 1.20}

	var createCalls []string
	var updatedOffer cloud.Offer
	var progress []string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, _ cloud.CreateOpts) (*cloud.Instance, error) {
			createCalls = append(createCalls, offerID)
			if len(createCalls) == 1 {
				return nil, fmt.Errorf("%w: ask 999 no longer exists", cloud.ErrOfferUnavailable)
			}
			return &cloud.Instance{ProviderID: "inst-123", Status: "creating"}, nil
		},
	}

	inst, finalOffer, err := createInstanceWithReplacement(
		mockClient,
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(phase string) { progress = append(progress, phase) },
		func(offer cloud.Offer) error {
			updatedOffer = offer
			return nil
		},
		func(failedOffer cloud.Offer) (*cloud.Offer, error) {
			if failedOffer.ProviderID != initialOffer.ProviderID {
				t.Fatalf("failed offer ID = %s, want %s", failedOffer.ProviderID, initialOffer.ProviderID)
			}
			return &replacement, nil
		},
	)
	if err != nil {
		t.Fatalf("createInstanceWithReplacement: %v", err)
	}
	if inst == nil || inst.ProviderID != "inst-123" {
		t.Fatalf("instance = %+v, want inst-123", inst)
	}
	if finalOffer.ProviderID != replacement.ProviderID {
		t.Fatalf("final offer ID = %s, want %s", finalOffer.ProviderID, replacement.ProviderID)
	}
	if updatedOffer.ProviderID != replacement.ProviderID {
		t.Fatalf("updated offer ID = %s, want %s", updatedOffer.ProviderID, replacement.ProviderID)
	}
	if got := strings.Join(createCalls, ","); got != "999,1001" {
		t.Fatalf("create calls = %q, want %q", got, "999,1001")
	}
	if got := strings.Join(progress, " | "); got != "creating instance | offer disappeared; searching again | retrying with replacement offer" {
		t.Fatalf("progress = %q", got)
	}
}

func TestCreateInstanceWithReplacementRetriesUnavailableRunpodOffer(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24}
	initialOffer := cloud.Offer{ProviderID: "RTX4090", Provider: cloud.ProviderRunpod, CostPerHour: 0.80}
	replacement := cloud.Offer{ProviderID: "RTX4090-B", Provider: cloud.ProviderRunpod, CostPerHour: 0.90}

	var createCalls []string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderRunpod,
		CreateInstanceFunc: func(offerID string, _ cloud.CreateOpts) (*cloud.Instance, error) {
			createCalls = append(createCalls, offerID)
			if len(createCalls) == 1 {
				return nil, fmt.Errorf("%w: gpu %s no longer exists", cloud.ErrOfferUnavailable, offerID)
			}
			return &cloud.Instance{ProviderID: "pod-123", Status: "creating"}, nil
		},
	}

	inst, finalOffer, err := createInstanceWithReplacement(
		mockClient,
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(string) {},
		nil,
		func(cloud.Offer) (*cloud.Offer, error) { return &replacement, nil },
	)
	if err != nil {
		t.Fatalf("createInstanceWithReplacement: %v", err)
	}
	if inst == nil || inst.ProviderID != "pod-123" {
		t.Fatalf("instance = %+v, want pod-123", inst)
	}
	if finalOffer.ProviderID != replacement.ProviderID {
		t.Fatalf("final offer ID = %s, want %s", finalOffer.ProviderID, replacement.ProviderID)
	}
	if got := strings.Join(createCalls, ","); got != "RTX4090,RTX4090-B" {
		t.Fatalf("create calls = %q, want %q", got, "RTX4090,RTX4090-B")
	}
}

func TestCreateInstanceWithReplacementNoReplacementOffer(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24}
	initialOffer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai, CostPerHour: 1.00}

	var replacementCalled bool
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, _ cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, fmt.Errorf("%w: ask %s no longer exists", cloud.ErrOfferUnavailable, offerID)
		},
	}

	_, _, err := createInstanceWithReplacement(
		mockClient,
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(string) {},
		func(cloud.Offer) error {
			t.Fatal("metadata update should not be called")
			return nil
		},
		func(cloud.Offer) (*cloud.Offer, error) {
			replacementCalled = true
			return nil, nil
		},
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !replacementCalled {
		t.Fatal("replacement callback was not called")
	}
	if !strings.Contains(err.Error(), "no replacement offer found") {
		t.Fatalf("error = %v, want no replacement offer found", err)
	}
}

func TestCreateInstanceWithReplacementDoesNotRetryGenericError(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24}
	initialOffer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai, CostPerHour: 1.00}

	replacementCalled := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, errors.New("API error: insufficient balance")
		},
	}

	_, _, err := createInstanceWithReplacement(
		mockClient,
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(string) {},
		nil,
		func(cloud.Offer) (*cloud.Offer, error) {
			replacementCalled = true
			return nil, nil
		},
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if replacementCalled {
		t.Fatal("replacement callback should not be called")
	}
	if !strings.Contains(err.Error(), "insufficient balance") {
		t.Fatalf("error = %v, want insufficient balance", err)
	}
}

func TestReplacementOfferAllowed(t *testing.T) {
	failed := cloud.Offer{CostPerHour: 1.00}
	if !replacementOfferAllowed(failed, cloud.Offer{CostPerHour: 1.25}) {
		t.Fatal("expected replacement at 25% premium to be allowed")
	}
	if replacementOfferAllowed(failed, cloud.Offer{CostPerHour: 1.26}) {
		t.Fatal("expected replacement above 25% premium to be rejected")
	}
}
