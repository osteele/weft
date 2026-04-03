package campaign

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
				Status:      cloud.ProviderStatusRunning,
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
	if !errors.Is(err, ErrR2ClientRequired) {
		t.Errorf("expected ErrR2ClientRequired, got: %v", err)
	}

	// Verify CreateInstance was NOT called (we fail before that)
	if createdOfferID != "" {
		t.Errorf("expected no CreateInstance call, got offer ID %q", createdOfferID)
	}
}

func TestLaunchInstanceCreateFails(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	errInsufficientBalance := errors.New("API error: insufficient balance")
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, errInsufficientBalance
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
	if !errors.Is(err, errInsufficientBalance) {
		t.Errorf("expected errInsufficientBalance, got: %v", err)
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

	errStopAfterRegistration := errors.New("stop after registration")
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, errStopAfterRegistration
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
	if err == nil || !errors.Is(err, errStopAfterRegistration) {
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
	if !errors.Is(err, ErrNoLaunchGroups) {
		t.Fatalf("expected ErrNoLaunchGroups, got: %v", err)
	}

	var campaigns int
	if scanErr := database.QueryRow(`SELECT COUNT(*) FROM campaigns`).Scan(&campaigns); scanErr != nil {
		t.Fatalf("count campaigns: %v", scanErr)
	}
	if campaigns != 0 {
		t.Fatalf("campaign count = %d, want 0", campaigns)
	}
}

func TestLaunchCampaign_AutoFailsOnFirstRegistrationTimeout(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, '/tmp', ?, ?, ?, 0)`,
		101, "RTX_4090", 24, "python train.py",
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs: []*db.Job{
			{
				ID:         101,
				Status:     db.StatusQueued,
				Command:    "python train.py",
				WorkingDir: "/tmp",
			},
		},
	}
	offer := cloud.Offer{
		Provider:    cloud.ProviderVastai,
		ProviderID:  "offer-1",
		GPUName:     "RTX_4090",
		NumGPUs:     1,
		CostPerHour: 0.5,
		DataCenter:  "us-east",
	}

	mockClient := &cloud.MockClient{ProviderVal: cloud.ProviderVastai}
	stager := &R2AssetStager{
		Client:         &r2.Client{},
		AgentVersion:   "test",
		agentKey:       newStringPromise(),
		sourcePromises: map[string]*stringPromise{},
	}
	var closeOnce sync.Once
	stager.cancel = func() {
		closeOnce.Do(func() {
			stager.agentKey.resolve("", errors.New("stager canceled"))
		})
	}

	var campaignID int64
	result, err := LaunchCampaignWithAssetStager(
		stager,
		[]cloud.Client{mockClient},
		database,
		[]InstanceGroup{group},
		[]cloud.Offer{offer},
		nil,
		nil,
		LaunchOpts{},
		cloud.R2Config{},
		func(cloud.Provider) (cloud.CreateOpts, error) { return cloud.CreateOpts{}, nil },
		func(InstanceGroup, string) {},
		func(id int64) {
			campaignID = id
			// Force elapsed time past first-registration terminate threshold.
			_, _ = database.Exec(`UPDATE campaigns SET created_at = ? WHERE id = ?`, time.Now().Add(-24*time.Hour).Unix(), id)
		},
		nil,
	)
	if err == nil {
		t.Fatal("expected first-registration timeout error")
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil on timeout", result)
	}
	if !strings.Contains(err.Error(), "first worker registration timed out") {
		t.Fatalf("error = %v, want first-registration timeout", err)
	}
	if campaignID == 0 {
		t.Fatal("expected campaign ID from callback")
	}

	c, getErr := db.GetCampaign(database, campaignID)
	if getErr != nil {
		t.Fatalf("get campaign: %v", getErr)
	}
	if c == nil {
		t.Fatal("campaign not found")
	}
	if c.Status != db.CampaignStatusFailed {
		t.Fatalf("campaign status = %q, want %q", c.Status, db.CampaignStatusFailed)
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
			return &cloud.Instance{ProviderID: "inst-123", Status: cloud.ProviderStatusCreating}, nil
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
		func(excludeKeys map[string]struct{}) (*cloud.Offer, error) {
			if _, ok := excludeKeys[initialOffer.Key()]; !ok {
				t.Fatalf("exclude set should contain initial offer key %s", initialOffer.Key())
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
	if got := strings.Join(progress, " | "); !strings.Contains(got, "creating instance") || !strings.Contains(got, "failed; searching again") {
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
			return &cloud.Instance{ProviderID: "pod-123", Status: cloud.ProviderStatusCreating}, nil
		},
	}

	inst, finalOffer, err := createInstanceWithReplacement(
		mockClient,
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(string) {},
		nil,
		func(map[string]struct{}) (*cloud.Offer, error) { return &replacement, nil },
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
		func(map[string]struct{}) (*cloud.Offer, error) {
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
	if !errors.Is(err, ErrNoReplacementOffer) {
		t.Fatalf("expected ErrNoReplacementOffer, got: %v", err)
	}
}

func TestCreateInstanceWithReplacementDoesNotRetryGenericError(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24}
	initialOffer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai, CostPerHour: 1.00}

	errInsufficientBalance := errors.New("API error: insufficient balance")
	replacementCalled := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, errInsufficientBalance
		},
	}

	_, _, err := createInstanceWithReplacement(
		mockClient,
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(string) {},
		nil,
		func(map[string]struct{}) (*cloud.Offer, error) {
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
	if !errors.Is(err, errInsufficientBalance) {
		t.Fatalf("expected errInsufficientBalance, got: %v", err)
	}
}

func TestCreateInstanceWithReplacementRetriesProviderRejected(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24}
	offers := []cloud.Offer{
		{ProviderID: "offer-1", Provider: cloud.ProviderVastai, CostPerHour: 1.00},
		{ProviderID: "offer-2", Provider: cloud.ProviderVastai, CostPerHour: 1.10},
		{ProviderID: "offer-3", Provider: cloud.ProviderVastai, CostPerHour: 1.15},
	}

	var createCalls []string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, _ cloud.CreateOpts) (*cloud.Instance, error) {
			createCalls = append(createCalls, offerID)
			if len(createCalls) <= 2 {
				return nil, fmt.Errorf("create instance failed: %w: provider returned success=false", cloud.ErrProviderRejected)
			}
			return &cloud.Instance{ProviderID: "inst-ok", Status: cloud.ProviderStatusCreating}, nil
		},
	}

	replacementIdx := 0
	inst, finalOffer, err := createInstanceWithReplacement(
		mockClient,
		group,
		offers[0],
		cloud.CreateOpts{},
		func(string) {},
		nil,
		func(excludeKeys map[string]struct{}) (*cloud.Offer, error) {
			replacementIdx++
			if replacementIdx >= len(offers) {
				return nil, nil
			}
			return &offers[replacementIdx], nil
		},
	)
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if inst == nil || inst.ProviderID != "inst-ok" {
		t.Fatalf("instance = %+v, want inst-ok", inst)
	}
	if finalOffer.ProviderID != "offer-3" {
		t.Fatalf("final offer = %s, want offer-3", finalOffer.ProviderID)
	}
	if got := strings.Join(createCalls, ","); got != "offer-1,offer-2,offer-3" {
		t.Fatalf("create calls = %q, want %q", got, "offer-1,offer-2,offer-3")
	}
}

func TestCreateInstanceWithReplacementExhaustsAttempts(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24}
	initialOffer := cloud.Offer{ProviderID: "offer-1", Provider: cloud.ProviderVastai, CostPerHour: 1.00}

	offerIdx := 1
	var createCalls int
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
			createCalls++
			return nil, fmt.Errorf("create instance failed: %w: provider returned success=false", cloud.ErrProviderRejected)
		},
	}

	_, _, err := createInstanceWithReplacement(
		mockClient,
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(string) {},
		nil,
		func(map[string]struct{}) (*cloud.Offer, error) {
			offerIdx++
			return &cloud.Offer{
				ProviderID:  fmt.Sprintf("offer-%d", offerIdx),
				Provider:    cloud.ProviderVastai,
				CostPerHour: 1.00,
			}, nil
		},
	)
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if createCalls != maxCreateAttempts {
		t.Fatalf("create calls = %d, want %d", createCalls, maxCreateAttempts)
	}
}

func TestReplacementPriceAllowed(t *testing.T) {
	if !replacementPriceAllowed(1.00, 1.25) {
		t.Fatal("expected replacement at 25% premium to be allowed")
	}
	if replacementPriceAllowed(1.00, 1.26) {
		t.Fatal("expected replacement above 25% premium to be rejected")
	}
}
