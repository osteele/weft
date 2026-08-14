package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/retrypolicy"
	weftsync "github.com/osteele/weft/internal/sync"
)

// setupTestDB creates an in-memory SQLite database for testing.
func setupTestDB(t *testing.T) *sql.DB {
	return db.SetupTestDB(t)
}

type runpodDriverProbeMockClient struct {
	*cloud.MockClient
	probeOutput string
	probeErr    error
	probedPodID string
}

func (m *runpodDriverProbeMockClient) ProbeDriverVersion(_ context.Context, podID string) (string, error) {
	m.probedPodID = podID
	return m.probeOutput, m.probeErr
}

func TestRunPodSSHBootstrapTimeoutPrecedesLaunchingWatchdog(t *testing.T) {
	if runpodSSHBootstrapTimeout >= launchingPhaseTimeout {
		t.Fatalf("runpodSSHBootstrapTimeout = %s, must be below launchingPhaseTimeout = %s", runpodSSHBootstrapTimeout, launchingPhaseTimeout)
	}
}

func TestConfigureBootstrapCreateOpts_RunpodKeepsTemplate(t *testing.T) {
	client := &cloud.MockClient{ProviderVal: cloud.ProviderRunpod}
	opts := cloud.CreateOpts{
		TemplateID: "tpl-bootstrap",
		OnStartCmd: "echo should be cleared",
	}
	if err := configureBootstrapCreateOpts(client, &opts, "bootstrap/1.sh"); err != nil {
		t.Fatalf("configureBootstrapCreateOpts: %v", err)
	}
	if opts.TemplateID != "tpl-bootstrap" {
		t.Fatalf("TemplateID = %q, want tpl-bootstrap", opts.TemplateID)
	}
	if opts.OnStartCmd != "" {
		t.Fatalf("OnStartCmd = %q, want empty", opts.OnStartCmd)
	}
	if opts.EnvVars[cloud.R2BootstrapKeyEnvVar] != "bootstrap/1.sh" {
		t.Fatalf("%s = %q, want bootstrap/1.sh", cloud.R2BootstrapKeyEnvVar, opts.EnvVars[cloud.R2BootstrapKeyEnvVar])
	}
}

func TestApplyGroupCreateRequirements_RunpodCloudType(t *testing.T) {
	opts := cloud.CreateOpts{}
	applyGroupCreateRequirements(&opts, InstanceGroup{RunpodCloudType: cloud.RunpodCloudTypeSecure})
	if opts.RunpodCloudType != cloud.RunpodCloudTypeSecure {
		t.Fatalf("RunpodCloudType = %q, want secure", opts.RunpodCloudType)
	}
}

func TestAddRunpodAPIKeyEnvUsesConfigBackedResolver(t *testing.T) {
	orig := runpodAPIKeyForLaunch
	t.Cleanup(func() { runpodAPIKeyForLaunch = orig })
	runpodAPIKeyForLaunch = func() (string, error) {
		return "from-config", nil
	}

	env := map[string]string{}
	addRunpodAPIKeyEnv(env, 123)

	if env["RUNPOD_API_KEY"] != "from-config" {
		t.Fatalf("RUNPOD_API_KEY = %q, want from-config", env["RUNPOD_API_KEY"])
	}
}

func TestCreateInstanceWithReplacement_PreservesExplicitGPUCount(t *testing.T) {
	var gotGPUCount int
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(_ string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			gotGPUCount = opts.GPUCount
			return &cloud.Instance{ProviderID: "inst-1"}, nil
		},
	}

	_, _, _, err := createInstanceWithReplacement(
		client,
		func(cloud.Provider) cloud.Client { return client },
		InstanceGroup{NumGPUs: 2},
		cloud.Offer{ProviderID: "offer-1", NumGPUs: 1},
		cloud.CreateOpts{GPUCount: 2},
		func(string) {},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("createInstanceWithReplacement: %v", err)
	}
	if gotGPUCount != 2 {
		t.Fatalf("GPUCount = %d, want explicit group count 2", gotGPUCount)
	}
}

func TestAssignAgentGPUSlots(t *testing.T) {
	jobs := []cloud.AgentJob{{ID: 10}, {ID: 11}}

	assignAgentGPUSlots(InstanceGroup{SlotGPUs: true}, jobs)

	if jobs[0].GPU != "0" || jobs[1].GPU != "1" {
		t.Fatalf("assigned GPUs = %q, %q; want 0, 1", jobs[0].GPU, jobs[1].GPU)
	}
	if !jobs[0].SlotGPU || !jobs[1].SlotGPU {
		t.Fatal("assigned jobs should carry SlotGPU=true")
	}
	if jobs[0].GPUCount != 1 || jobs[1].GPUCount != 1 {
		t.Fatalf("GPUCount = %d, %d; want 1, 1", jobs[0].GPUCount, jobs[1].GPUCount)
	}
}

func TestSlottedLaunchUsesPerJobRemoteDirsAndSources(t *testing.T) {
	local := t.TempDir()
	group := InstanceGroup{
		SlotGPUs: true,
		Jobs: []*db.Job{
			{ID: 10, WorkingDir: local},
			{ID: 11, WorkingDir: local},
		},
	}
	localToRemote := map[string]string{local: baseRemoteDirForLocal(local)}

	dir10 := remoteDirForLaunchAgentJob(group, group.Jobs[0], localToRemote)
	dir11 := remoteDirForLaunchAgentJob(group, group.Jobs[1], localToRemote)
	if dir10 == dir11 {
		t.Fatalf("slotted jobs share remote dir %q", dir10)
	}

	sources := sourceMappingsForLaunch(group, localToRemote, nil, nil, map[string]string{local: "sources/src.tar.gz"})
	if len(sources) != 2 {
		t.Fatalf("source mappings = %d, want 2", len(sources))
	}
	if sources[0].RemoteDir == sources[1].RemoteDir {
		t.Fatalf("source mappings share remote dir %q", sources[0].RemoteDir)
	}
	if sources[0].R2Key != "sources/src.tar.gz" || sources[1].R2Key != "sources/src.tar.gz" {
		t.Fatalf("source R2 keys = %q, %q; want same source tarball", sources[0].R2Key, sources[1].R2Key)
	}
}

func TestLaunchInstanceRejectsOfferBelowGroupGPUCount(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	client := &cloud.MockClient{ProviderVal: cloud.ProviderVastai}
	_, err := LaunchInstance(
		client, nil, database, nil,
		InstanceGroup{GPUClass: "A100", GPUMemGB: 80, NumGPUs: 2},
		cloud.Offer{ProviderID: "one-gpu", GPUName: "A100 SXM4", GPUMemGB: 80, NumGPUs: 1},
		LaunchOpts{},
		cloud.R2Config{Bucket: "test"},
		cloud.CreateOpts{},
		R2Assets{Client: &r2.Client{}},
		nil,
		func(string) {},
		nil,
	)
	if err == nil {
		t.Fatal("LaunchInstance unexpectedly accepted one-GPU offer for two-GPU group")
	}
	if !strings.Contains(err.Error(), "offer GPU count insufficient: need=2 offer=1") {
		t.Fatalf("error = %v, want GPU count insufficiency", err)
	}
}

func TestLaunchInstanceRejectsExternalJobBeforeCreatingRental(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	job := &db.Job{ID: 101, Backend: db.BackendSkyPilot, Status: db.StatusQueued, Command: "python train.py"}
	_, err := LaunchInstance(
		&cloud.MockClient{ProviderVal: cloud.ProviderVastai}, nil, database, nil,
		InstanceGroup{GPUClass: "A100", GPUMemGB: 80, Jobs: []*db.Job{job}},
		cloud.Offer{ProviderID: "offer", Provider: cloud.ProviderVastai, GPUName: "A100", GPUMemGB: 80, NumGPUs: 1},
		LaunchOpts{}, cloud.R2Config{}, cloud.CreateOpts{}, R2Assets{}, nil, nil, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "managed by SkyPilot") {
		t.Fatalf("LaunchInstance external-job error = %v", err)
	}
	launches, listErr := db.ListLaunches(database)
	if listErr != nil {
		t.Fatalf("list launches: %v", listErr)
	}
	if len(launches) != 0 {
		t.Fatalf("external job created %d launch rows", len(launches))
	}
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
		mockClient, nil, database, nil, group, offer,
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
		mockClient, nil, database, nil, group, offer,
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

func TestLaunchInstanceCreateTimesOutMarksProviderTimeout(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Wrap the timeout sentinel the way vastai/client.go:521 does at runtime,
	// so the launch-failure site sees it through errors.Is.
	errTimeout := fmt.Errorf("%w: vastai create instance 12345 timed out after 2m0s", cloud.ErrProviderCommandTimeout)
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, errTimeout
		},
	}

	job := &db.Job{
		ID:      102,
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
		mockClient, nil, database, nil, group, offer,
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
	if !errors.Is(err, cloud.ErrProviderCommandTimeout) {
		t.Errorf("expected ErrProviderCommandTimeout, got: %v", err)
	}

	ci, getErr := db.GetLaunch(database, instanceID)
	if getErr != nil {
		t.Fatalf("get cloud instance: %v", getErr)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
	if ci.TerminationReason != db.TerminationReasonProviderTimeout {
		t.Fatalf("termination reason = %q, want %q (provider timeouts must not be tagged as generic infra_failure — they drive the clustered-failures banner filter)", ci.TerminationReason, db.TerminationReasonProviderTimeout)
	}
}

func TestLaunchInstanceRunpodDistinctMachineConflictHoldsBlocker(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{
		Status:           db.CampaignStatusLaunching,
		DistinctMachines: true,
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	priorID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusRunning,
		Provider:   string(cloud.ProviderRunpod),
		MachineID:  "machine-1",
	})
	if err != nil {
		t.Fatalf("CreateLaunch prior: %v", err)
	}
	if priorID == 0 {
		t.Fatal("prior launch id = 0")
	}

	job := &db.Job{
		ID:      103,
		Status:  db.StatusQueued,
		Command: "python train.py",
	}
	group := InstanceGroup{
		GPUClass: "L4",
		GPUMemGB: 24,
		Jobs:     []*db.Job{job},
	}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, '/tmp', ?, ?, ?, 0)`,
		job.ID, group.GPUClass, group.GPUMemGB, job.Command,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	var destroyed []string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderRunpod,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			return &cloud.Instance{
				ProviderID: "pod-duplicate",
				Provider:   cloud.ProviderRunpod,
				Status:     cloud.ProviderStatusRunning,
				MachineID:  "machine-1",
			}, nil
		},
		ShowInstanceFunc: func(instanceID string) (*cloud.Instance, error) {
			return &cloud.Instance{
				ProviderID: instanceID,
				Provider:   cloud.ProviderRunpod,
				Status:     cloud.ProviderStatusRunning,
				MachineID:  "machine-1",
			}, nil
		},
		DestroyInstanceFunc: func(instanceID string) error {
			destroyed = append(destroyed, instanceID)
			return nil
		},
	}

	instanceID, err := LaunchInstance(
		mockClient, []cloud.Client{mockClient}, database, &campaignID, group,
		cloud.Offer{ProviderID: "L4", Provider: cloud.ProviderRunpod, GPUName: "L4", GPUMemGB: 24},
		LaunchOpts{DistinctMachines: true},
		cloud.R2Config{Bucket: "test", AccountID: "test"},
		cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"},
		R2Assets{Client: &r2.Client{}},
		nil,
		func(string) {},
		nil,
	)
	if err == nil {
		t.Fatal("expected distinct-machine conflict error, got nil")
	}
	var conflictErr *distinctMachinePostCreateConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("error = %v, want distinctMachinePostCreateConflictError", err)
	}
	if !errors.Is(err, cloud.ErrOfferUnavailable) {
		t.Fatalf("errors.Is(err, ErrOfferUnavailable) = false")
	}
	if len(destroyed) != 0 {
		t.Fatalf("destroyed = %#v, want held blocker", destroyed)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.Status != db.LaunchStatusLaunching {
		t.Fatalf("status = %q, want launching held blocker", ci.Status)
	}
	if !ci.Cordoned || !strings.Contains(ci.CordonReason, "already held") {
		t.Fatalf("cordon = %v %q, want duplicate-machine reason", ci.Cordoned, ci.CordonReason)
	}
	if ci.EffectiveProviderID() != "pod-duplicate" || ci.MachineID != "machine-1" {
		t.Fatalf("provider/machine = %q/%q, want pod-duplicate/machine-1", ci.EffectiveProviderID(), ci.MachineID)
	}
	resetJob, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if resetJob.LaunchID != nil {
		t.Fatalf("job launch = %v, want nil after blocker rejection", *resetJob.LaunchID)
	}
}

func TestLaunchInstanceRunpodDriverCompatibilityRejectsOldDriver(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	job := &db.Job{
		ID:      104,
		Status:  db.StatusQueued,
		Command: "python train.py",
	}
	group := InstanceGroup{
		GPUClass:         "L4",
		GPUMemGB:         24,
		MinDriverVersion: 570,
		Jobs:             []*db.Job{job},
	}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, '/tmp', ?, ?, ?, 0)`,
		job.ID, group.GPUClass, group.GPUMemGB, job.Command,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	var destroyed []string
	client := &runpodDriverProbeMockClient{
		MockClient: &cloud.MockClient{
			ProviderVal: cloud.ProviderRunpod,
			CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
				return &cloud.Instance{
					ProviderID: "pod-old-driver",
					Provider:   cloud.ProviderRunpod,
					Status:     cloud.ProviderStatusRunning,
					MachineID:  "machine-old",
				}, nil
			},
			ShowInstanceFunc: func(instanceID string) (*cloud.Instance, error) {
				return &cloud.Instance{
					ProviderID: instanceID,
					Provider:   cloud.ProviderRunpod,
					Status:     cloud.ProviderStatusRunning,
					MachineID:  "machine-old",
				}, nil
			},
			DestroyInstanceFunc: func(instanceID string) error {
				destroyed = append(destroyed, instanceID)
				return nil
			},
		},
		probeOutput: "550.127.05\n",
	}

	instanceID, err := LaunchInstance(
		client, []cloud.Client{client}, database, nil, group,
		cloud.Offer{ProviderID: "L4", Provider: cloud.ProviderRunpod, GPUName: "NVIDIA L4", GPUMemGB: 24},
		LaunchOpts{},
		cloud.R2Config{Bucket: "test", AccountID: "test"},
		cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"},
		R2Assets{Client: &r2.Client{}},
		nil,
		func(string) {},
		nil,
	)
	if err == nil {
		t.Fatal("expected driver compatibility error, got nil")
	}
	var driverErr *postCreateDriverCompatibilityError
	if !errors.As(err, &driverErr) {
		t.Fatalf("error = %v, want postCreateDriverCompatibilityError", err)
	}
	if driverErr.KeepAlive {
		t.Fatal("KeepAlive = true, want false for direct launch rejection")
	}
	if !errors.Is(err, cloud.ErrOfferUnavailable) {
		t.Fatalf("errors.Is(err, ErrOfferUnavailable) = false")
	}
	if client.probedPodID != "pod-old-driver" {
		t.Fatalf("probedPodID = %q, want pod-old-driver", client.probedPodID)
	}
	if len(destroyed) != 1 || destroyed[0] != "pod-old-driver" {
		t.Fatalf("destroyed = %#v, want pod-old-driver", destroyed)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("status = %q, want failed", ci.Status)
	}
	if ci.TerminationReason != db.TerminationReasonInfraFailure {
		t.Fatalf("termination reason = %q, want infra_failure", ci.TerminationReason)
	}
	if ci.EffectiveProviderID() != "pod-old-driver" || ci.MachineID != "machine-old" {
		t.Fatalf("provider/machine = %q/%q, want pod-old-driver/machine-old", ci.EffectiveProviderID(), ci.MachineID)
	}
	state, err := db.GetLaunchLiveState(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if state == nil || state.InstancePhase != phaseDriverTooOld {
		t.Fatalf("instance phase = %#v, want %q", state, phaseDriverTooOld)
	}
	resetJob, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if resetJob.LaunchID != nil {
		t.Fatalf("job launch = %v, want nil after driver rejection", *resetJob.LaunchID)
	}
	attempts, err := db.GetLaunchAttempts(database, job.ID)
	if err != nil {
		t.Fatalf("GetLaunchAttempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != db.AttemptOutcomeOrphaned {
		t.Fatalf("attempts = %#v, want one orphaned attempt", attempts)
	}
}

func TestLaunchInstanceRunpodDriverCompatibilityHoldsBlockerForReplan(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	job := &db.Job{
		ID:      105,
		Status:  db.StatusQueued,
		Command: "python train.py",
	}
	group := InstanceGroup{
		GPUClass:         "L4",
		GPUMemGB:         24,
		MinDriverVersion: 570,
		Jobs:             []*db.Job{job},
	}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, '/tmp', ?, ?, ?, 0)`,
		job.ID, group.GPUClass, group.GPUMemGB, job.Command,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	var destroyed []string
	client := &runpodDriverProbeMockClient{
		MockClient: &cloud.MockClient{
			ProviderVal: cloud.ProviderRunpod,
			CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
				return &cloud.Instance{
					ProviderID: "pod-old-driver",
					Provider:   cloud.ProviderRunpod,
					Status:     cloud.ProviderStatusRunning,
					MachineID:  "machine-old",
				}, nil
			},
			ShowInstanceFunc: func(instanceID string) (*cloud.Instance, error) {
				return &cloud.Instance{
					ProviderID: instanceID,
					Provider:   cloud.ProviderRunpod,
					Status:     cloud.ProviderStatusRunning,
					MachineID:  "machine-old",
				}, nil
			},
			DestroyInstanceFunc: func(instanceID string) error {
				destroyed = append(destroyed, instanceID)
				return nil
			},
		},
		probeOutput: "550.127.05\n",
	}

	instanceID, err := LaunchInstance(
		client, []cloud.Client{client}, database, nil, group,
		cloud.Offer{ProviderID: "L4", Provider: cloud.ProviderRunpod, GPUName: "NVIDIA L4", GPUMemGB: 24},
		LaunchOpts{holdRunpodDriverBlockers: true},
		cloud.R2Config{Bucket: "test", AccountID: "test"},
		cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"},
		R2Assets{Client: &r2.Client{}},
		nil,
		func(string) {},
		nil,
	)
	if err == nil {
		t.Fatal("expected driver compatibility error, got nil")
	}
	var driverErr *postCreateDriverCompatibilityError
	if !errors.As(err, &driverErr) {
		t.Fatalf("error = %v, want postCreateDriverCompatibilityError", err)
	}
	if !driverErr.KeepAlive {
		t.Fatal("KeepAlive = false, want true for replan-aware launch")
	}
	if len(destroyed) != 0 {
		t.Fatalf("destroyed = %#v, want held blocker", destroyed)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.Status != db.LaunchStatusLaunching {
		t.Fatalf("status = %q, want launching held blocker", ci.Status)
	}
	if !ci.Cordoned || !strings.Contains(ci.CordonReason, "below required major 570") {
		t.Fatalf("cordon = %v %q, want driver rejection reason", ci.Cordoned, ci.CordonReason)
	}
	resetJob, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if resetJob.LaunchID != nil {
		t.Fatalf("job launch = %v, want nil after blocker rejection", *resetJob.LaunchID)
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
		mockClient, nil, database, nil, group, offer,
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

// Regression: Instance.gpu_mem_gb must record the offer's actual per-GPU
// memory, not the job group's minimum memory requirement. Otherwise a
// running rental's GPUMemGB is 0 in the DB and the reuse matcher rejects
// new jobs with memory constraints.
func TestLaunchInstanceRecordsOfferGPUMemory(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	errStop := errors.New("stop after registration")
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, errStop
		},
	}

	job := &db.Job{ID: 701, Status: db.StatusQueued, Command: "python train.py"}
	group := InstanceGroup{
		GPUClass: "RTX_3090",
		GPUMemGB: 20, // job constraint: needs at least 20GB
		Jobs:     []*db.Job{job},
	}
	// Offer is the actual hardware — a 24GB RTX 3090 satisfies the 20GB constraint.
	offer := cloud.Offer{
		ProviderID: "900",
		Provider:   cloud.ProviderVastai,
		GPUName:    "RTX_3090",
		GPUMemGB:   24,
	}
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
		mockClient, nil, database, nil, group, offer,
		LaunchOpts{}, r2Cfg, createOpts,
		R2Assets{Client: &r2.Client{}},
		nil, func(string) {}, nil,
	)
	if err == nil || !errors.Is(err, errStop) {
		t.Fatalf("LaunchInstance() error = %v, want stop-after-registration failure", err)
	}
	if instanceID == 0 {
		t.Fatal("expected DB instance ID")
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.GPUMemGB != 24 {
		t.Fatalf("Instance.GPUMemGB = %d, want 24 (actual offer memory, not job constraint)", ci.GPUMemGB)
	}
}

func TestLaunchInstanceRecomputesDiskFloor(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	errStop := errors.New("stop after registration")
	var gotCreateOpts cloud.CreateOpts
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(_ string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			gotCreateOpts = opts
			return nil, errStop
		},
	}

	job := &db.Job{
		ID:         771,
		Status:     db.StatusQueued,
		WorkingDir: "/tmp/project",
		Command:    "python train.py",
		Metadata: &db.JobMetadata{
			Disk: &db.JobDiskMetadata{DiskGB: 96},
		},
	}
	group := InstanceGroup{
		GPUClass: "RTX_3090",
		GPUMemGB: 20,
		Jobs:     []*db.Job{job},
	}
	offer := cloud.Offer{
		ProviderID: "900",
		Provider:   cloud.ProviderVastai,
		GPUName:    "RTX_3090",
		GPUMemGB:   24,
	}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		job.ID, job.WorkingDir, group.GPUClass, group.GPUMemGB, job.Command,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	instanceID, err := LaunchInstance(
		mockClient, nil, database, nil, group, offer,
		LaunchOpts{},
		cloud.R2Config{Bucket: "test", AccountID: "test"},
		cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04", DiskGB: 50},
		R2Assets{Client: &r2.Client{}},
		nil, func(string) {}, nil,
	)
	if err == nil || !errors.Is(err, errStop) {
		t.Fatalf("LaunchInstance() error = %v, want stop-after-registration failure", err)
	}
	if gotCreateOpts.DiskGB != 96 {
		t.Fatalf("CreateInstance DiskGB = %d, want 96", gotCreateOpts.DiskGB)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.DiskGB != 96 {
		t.Fatalf("Launch.DiskGB = %d, want 96", ci.DiskGB)
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
		sourcePromises: map[string]*sourceUploadPromise{},
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
		func(LaunchEvent) {},
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

	inst, finalOffer, _, err := createInstanceWithReplacement(
		mockClient,
		nil,
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

func TestCreateInstanceWithReplacementRejectsReplacementAboveJobRateCap(t *testing.T) {
	capCents := 110
	group := InstanceGroup{
		GPUClass: "RTX_4090",
		Jobs: []*db.Job{{
			CLIResourceOverrides: &db.CLIResourceOverrides{MaxHourlyRateCents: &capCents},
		}},
	}
	initialOffer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai, CostPerHour: 1.00}
	replacement := cloud.Offer{ProviderID: "1001", Provider: cloud.ProviderVastai, CostPerHour: 1.20}
	createCalls := 0
	metadataUpdates := 0
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
			createCalls++
			return nil, fmt.Errorf("%w: unavailable", cloud.ErrOfferUnavailable)
		},
	}

	_, _, _, err := createInstanceWithReplacement(
		client,
		nil,
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(string) {},
		func(cloud.Offer) error { metadataUpdates++; return nil },
		func(map[string]struct{}) (*cloud.Offer, error) { return &replacement, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds job max-hourly-rate") {
		t.Fatalf("error = %v, want hourly-rate rejection", err)
	}
	if createCalls != 1 {
		t.Fatalf("provider create calls = %d, want only the initial offer", createCalls)
	}
	if metadataUpdates != 0 {
		t.Fatalf("replacement metadata updates = %d, want 0", metadataUpdates)
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

	inst, finalOffer, _, err := createInstanceWithReplacement(
		mockClient,
		nil,
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

	_, _, _, err := createInstanceWithReplacement(
		mockClient,
		nil,
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

	_, _, _, err := createInstanceWithReplacement(
		mockClient,
		nil,
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
	inst, finalOffer, _, err := createInstanceWithReplacement(
		mockClient,
		nil,
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

	_, _, _, err := createInstanceWithReplacement(
		mockClient,
		nil,
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
	if createCalls != retrypolicy.MaxCreateAttempts() {
		t.Fatalf("create calls = %d, want %d", createCalls, retrypolicy.MaxCreateAttempts())
	}
}

// TestCreateInstanceWithReplacementSwapsClientOnCrossProviderOffer covers
// the cross-provider retry fallback: when the original provider's pool is
// dry but a different provider has a compatible offer within the price
// cap, createInstanceWithReplacement must (a) call the new provider's
// CreateInstance, not the old one, and (b) return the new client so the
// caller's downstream ShowInstance / DestroyInstance route correctly.
func TestCreateInstanceWithReplacementSwapsClientOnCrossProviderOffer(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24}
	initialOffer := cloud.Offer{ProviderID: "RTX4090", Provider: cloud.ProviderRunpod, CostPerHour: 0.40}
	crossProvider := cloud.Offer{ProviderID: "8765", Provider: cloud.ProviderVastai, CostPerHour: 0.45}

	var runpodCalls, vastaiCalls []string
	runpodClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderRunpod,
		CreateInstanceFunc: func(offerID string, _ cloud.CreateOpts) (*cloud.Instance, error) {
			runpodCalls = append(runpodCalls, offerID)
			return nil, fmt.Errorf("%w: gpu %s no longer exists", cloud.ErrOfferUnavailable, offerID)
		},
	}
	vastaiClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, _ cloud.CreateOpts) (*cloud.Instance, error) {
			vastaiCalls = append(vastaiCalls, offerID)
			return &cloud.Instance{ProviderID: "vast-inst-9", Status: cloud.ProviderStatusCreating}, nil
		},
	}
	lookup := func(p cloud.Provider) cloud.Client {
		switch p {
		case cloud.ProviderRunpod:
			return runpodClient
		case cloud.ProviderVastai:
			return vastaiClient
		}
		return nil
	}

	inst, finalOffer, finalClient, err := createInstanceWithReplacement(
		runpodClient,
		lookup,
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(string) {},
		nil,
		func(map[string]struct{}) (*cloud.Offer, error) { return &crossProvider, nil },
	)
	if err != nil {
		t.Fatalf("createInstanceWithReplacement: %v", err)
	}
	if inst == nil || inst.ProviderID != "vast-inst-9" {
		t.Fatalf("instance = %+v, want vast-inst-9 (from vastai client)", inst)
	}
	if finalOffer.ProviderID != crossProvider.ProviderID {
		t.Fatalf("final offer ID = %s, want %s", finalOffer.ProviderID, crossProvider.ProviderID)
	}
	if finalClient == nil || finalClient.Provider() != cloud.ProviderVastai {
		t.Fatalf("final client provider = %v, want vastai", finalClient)
	}
	if len(runpodCalls) != 1 || runpodCalls[0] != "RTX4090" {
		t.Fatalf("runpod create calls = %v, want one attempt against RTX4090", runpodCalls)
	}
	if len(vastaiCalls) != 1 || vastaiCalls[0] != "8765" {
		t.Fatalf("vastai create calls = %v, want one attempt against 8765 (the cross-provider replacement)", vastaiCalls)
	}
}

// TestCreateInstanceWithReplacementRejectsCrossProviderWhenLookupMissing
// guards against silently fanning out to a different provider when no
// cross-provider client lookup was supplied (e.g. relaunch and TUI single-
// shot paths pass nil for the lookup). The retry must fail loudly rather
// than calling the original provider's CreateInstance with an alien offer
// ID, which would burn an attempt and surface a confusing provider error.
func TestCreateInstanceWithReplacementRejectsCrossProviderWhenLookupMissing(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24}
	initialOffer := cloud.Offer{ProviderID: "RTX4090", Provider: cloud.ProviderRunpod, CostPerHour: 0.40}
	crossProvider := cloud.Offer{ProviderID: "8765", Provider: cloud.ProviderVastai, CostPerHour: 0.45}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderRunpod,
		CreateInstanceFunc: func(offerID string, _ cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, fmt.Errorf("%w: gpu %s no longer exists", cloud.ErrOfferUnavailable, offerID)
		},
	}

	_, _, _, err := createInstanceWithReplacement(
		mockClient,
		nil, // no lookup supplied — replacement must NOT be applied
		group,
		initialOffer,
		cloud.CreateOpts{},
		func(string) {},
		nil,
		func(map[string]struct{}) (*cloud.Offer, error) { return &crossProvider, nil },
	)
	if err == nil {
		t.Fatal("expected error when replacement is from another provider but lookup is nil")
	}
	if !strings.Contains(err.Error(), "no cross-provider client lookup was supplied") {
		t.Fatalf("err = %v, want it to mention the missing cross-provider lookup", err)
	}
}

// TestReplacementPriceAllowed and
// TestSearchReplacementOfferWithPriceCapSkipsExpensiveCandidates were
// removed when the fixed-multiplier cap (replacementPriceAllowed /
// searchReplacementOfferWithPriceCap) was replaced by the history-derived
// authorization gate. See price_anchor.go, price_gate.go, and their tests
// for the replacement's coverage.

// TestRunGroupLaunchWithReplan_SecondChainSucceedsAfterFirstExhausted is the
// happy-path replan scenario: the first chain returns a retryable error
// (e.g. createInstanceWithReplacement exhausted its inner budget), the
// fresh-offer search produces a different offer (potentially from a
// different provider), and the second chain succeeds. The helper must
// surface the second chain's outcome and the second offer/client.
func TestRunGroupLaunchWithReplan_SecondChainSucceedsAfterFirstExhausted(t *testing.T) {
	initialOffer := cloud.Offer{ProviderID: "RTX4090-A", Provider: cloud.ProviderRunpod, CostPerHour: 0.40}
	freshOffer := cloud.Offer{ProviderID: "8888", Provider: cloud.ProviderVastai, CostPerHour: 0.42}

	runpodClient := &cloud.MockClient{ProviderVal: cloud.ProviderRunpod}
	vastaiClient := &cloud.MockClient{ProviderVal: cloud.ProviderVastai}

	var launchCalls []cloud.Offer
	launch := func(c cloud.Client, o cloud.Offer) (int64, error) {
		launchCalls = append(launchCalls, o)
		if len(launchCalls) == 1 {
			// First chain exhausted: simulate the lastErr that
			// createInstanceWithReplacement would surface.
			return 0, fmt.Errorf("%w: ask %s no longer exists", cloud.ErrOfferUnavailable, o.ProviderID)
		}
		// Second chain: success
		return 555, nil
	}

	freshCalls := 0
	fetchFresh := func() (*cloud.Offer, error) {
		freshCalls++
		return &freshOffer, nil
	}
	lookup := func(p cloud.Provider) cloud.Client {
		switch p {
		case cloud.ProviderRunpod:
			return runpodClient
		case cloud.ProviderVastai:
			return vastaiClient
		}
		return nil
	}
	var progressMsgs []string
	progress := cloud.ProgressFunc(func(s string) { progressMsgs = append(progressMsgs, s) })

	cID, finalOffer, finalClient, err := runGroupLaunchWithReplan(
		initialOffer, runpodClient, 1, launch, fetchFresh, lookup, progress,
	)
	if err != nil {
		t.Fatalf("runGroupLaunchWithReplan: %v", err)
	}
	if cID != 555 {
		t.Fatalf("cID = %d, want 555 (second chain's success)", cID)
	}
	if finalOffer.ProviderID != freshOffer.ProviderID {
		t.Fatalf("finalOffer = %s, want %s (fresh offer)", finalOffer.ProviderID, freshOffer.ProviderID)
	}
	if finalClient.Provider() != cloud.ProviderVastai {
		t.Fatalf("finalClient provider = %v, want vastai", finalClient.Provider())
	}
	if len(launchCalls) != 2 {
		t.Fatalf("launch calls = %d, want 2 (one per chain)", len(launchCalls))
	}
	if freshCalls != 1 {
		t.Fatalf("fetchFresh calls = %d, want 1", freshCalls)
	}
	wantPhrase := "replanning with fresh offer (chain 2/2)"
	found := false
	for _, m := range progressMsgs {
		if strings.Contains(m, wantPhrase) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("progress missing replan phrase %q: %v", wantPhrase, progressMsgs)
	}
}

// TestRunGroupLaunchWithReplan_BothChainsExhaustBubblesUpFinalError verifies
// the budget-exhaustion path: when every chain fails retryably and the
// replan budget is spent, the helper returns the final error so the
// per-group goroutine can emit LaunchEventGroupFailed exactly once with
// the most recent terminal reason.
func TestRunGroupLaunchWithReplan_BothChainsExhaustBubblesUpFinalError(t *testing.T) {
	initialOffer := cloud.Offer{ProviderID: "A", Provider: cloud.ProviderVastai, CostPerHour: 0.40}
	freshOffer := cloud.Offer{ProviderID: "B", Provider: cloud.ProviderVastai, CostPerHour: 0.42}
	client := &cloud.MockClient{ProviderVal: cloud.ProviderVastai}

	var launchCalls []cloud.Offer
	chainErrs := []error{
		fmt.Errorf("chain1 exhausted: %w", cloud.ErrOfferUnavailable),
		fmt.Errorf("chain2 exhausted: %w", cloud.ErrOfferUnavailable),
	}
	launch := func(_ cloud.Client, o cloud.Offer) (int64, error) {
		launchCalls = append(launchCalls, o)
		return 0, chainErrs[len(launchCalls)-1]
	}
	fetchFresh := func() (*cloud.Offer, error) { return &freshOffer, nil }
	lookup := func(cloud.Provider) cloud.Client { return client }

	_, _, _, err := runGroupLaunchWithReplan(
		initialOffer, client, 1, launch, fetchFresh, lookup, nil,
	)
	if err == nil {
		t.Fatal("expected error after both chains exhaust")
	}
	if !strings.Contains(err.Error(), "chain2 exhausted") {
		t.Fatalf("err = %v, want second chain's terminal error", err)
	}
	if len(launchCalls) != 2 {
		t.Fatalf("launch calls = %d, want 2 (one per chain)", len(launchCalls))
	}
}

// TestRunGroupLaunchWithReplan_NonRetryableErrorSkipsReplan asserts the
// helper bails out immediately on errors that aren't classified as
// retryable — credit-exhaustion is the canonical case. Replanning against
// a fresh offer wouldn't help (the account still has no credit), so the
// error must propagate without a second chain.
func TestRunGroupLaunchWithReplan_NonRetryableErrorSkipsReplan(t *testing.T) {
	initialOffer := cloud.Offer{ProviderID: "A", Provider: cloud.ProviderVastai, CostPerHour: 0.40}
	client := &cloud.MockClient{ProviderVal: cloud.ProviderVastai}

	var launchCalls int
	launch := func(_ cloud.Client, _ cloud.Offer) (int64, error) {
		launchCalls++
		return 0, fmt.Errorf("create instance: %w", cloud.ErrAccountCreditExhausted)
	}
	freshCalls := 0
	fetchFresh := func() (*cloud.Offer, error) {
		freshCalls++
		return &initialOffer, nil
	}
	lookup := func(cloud.Provider) cloud.Client { return client }

	_, _, _, err := runGroupLaunchWithReplan(
		initialOffer, client, 1, launch, fetchFresh, lookup, nil,
	)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if !errors.Is(err, cloud.ErrAccountCreditExhausted) {
		t.Fatalf("err = %v, want credit-exhausted preserved", err)
	}
	if launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1 (no replan after non-retryable)", launchCalls)
	}
	if freshCalls != 0 {
		t.Fatalf("fetchFresh calls = %d, want 0", freshCalls)
	}
}

// TestRunGroupLaunchWithReplan_NoFreshOfferAvailableSurfacesChainError covers
// the case where the first chain fails retryably but the price-gated
// search returns no replacement (every candidate either disappeared or
// was rejected by the price gate). The helper must NOT call launch a
// second time and must surface the first chain's terminal error.
func TestRunGroupLaunchWithReplan_NoFreshOfferAvailableSurfacesChainError(t *testing.T) {
	initialOffer := cloud.Offer{ProviderID: "A", Provider: cloud.ProviderVastai, CostPerHour: 0.40}
	client := &cloud.MockClient{ProviderVal: cloud.ProviderVastai}

	launchCalls := 0
	launch := func(_ cloud.Client, _ cloud.Offer) (int64, error) {
		launchCalls++
		return 0, fmt.Errorf("first chain: %w", cloud.ErrOfferUnavailable)
	}
	fetchFresh := func() (*cloud.Offer, error) { return nil, nil }
	lookup := func(cloud.Provider) cloud.Client { return client }

	_, _, _, err := runGroupLaunchWithReplan(
		initialOffer, client, 1, launch, fetchFresh, lookup, nil,
	)
	if err == nil {
		t.Fatal("expected first-chain error to propagate")
	}
	if !strings.Contains(err.Error(), "first chain") {
		t.Fatalf("err = %v, want first chain's error", err)
	}
	if launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1 (no replan when fresh offer missing)", launchCalls)
	}
}

// TestClassifyLaunchGroupPhaseEvent_ReplanProgressBecomesGroupReplan
// covers the regex/classifier wiring: the progress phrase emitted by
// runGroupLaunchWithReplan must turn into a LaunchEventGroupReplan event
// with the chain numbers parsed, so the TUI ("chain N/M" badge) and
// move.go formatter can render it without scraping raw strings.
func TestClassifyLaunchGroupPhaseEvent_ReplanProgressBecomesGroupReplan(t *testing.T) {
	group := InstanceGroup{GPUClass: "RTX_4090"}
	event := classifyLaunchGroupPhaseEvent(group, "replanning with fresh offer (chain 2/3)")
	if event.Kind != LaunchEventGroupReplan {
		t.Fatalf("kind = %q, want %q", event.Kind, LaunchEventGroupReplan)
	}
	if event.RetryAttempt != 2 || event.RetryMax != 3 {
		t.Fatalf("attempt/max = %d/%d, want 2/3", event.RetryAttempt, event.RetryMax)
	}
}

func TestLaunchInstanceMoveTargetClaimKeepsSourceAuthoritative(t *testing.T) {
	// Regression: with LaunchOpts.MoveTargetClaim=true (move-to-new path),
	// the destination attempt is opened under the move intent without
	// closing the source. The provider-create failure below proves control
	// reached CreateInstance, then rollback leaves the source authoritative.
	database := setupTestDB(t)
	defer database.Close()

	src, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}

	job := &db.Job{ID: 201, Status: db.StatusQueued, Command: "python train.py"}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, '/tmp', 'RTX_4090', 24, ?, 0)`,
		job.ID, job.Command,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.SetJobLaunchID(database, job.ID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	sourceAttemptID, err := db.GetLatestAttemptID(database, job.ID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}

	stopAfterRegistration := errors.New("stop after registration")
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(string, cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, stopAfterRegistration
		},
	}
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24, Jobs: []*db.Job{job}}
	offer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai}

	// Stub the cancel-attempts sender so the test doesn't try to PutObject
	// against an empty stub r2.Client. The new protocol must not send this
	// marker before the target has accepted the job.
	prevSendCancel := sendGraceCancelAttempts
	t.Cleanup(func() { sendGraceCancelAttempts = prevSendCancel })
	canceledByLaunch := map[int64][]int64{}
	sendGraceCancelAttempts = func(_ context.Context, _ controlplane.GraceStore, instanceID int64, attemptIDs []int64) error {
		canceledByLaunch[instanceID] = append(canceledByLaunch[instanceID], attemptIDs...)
		return nil
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:         job.ID,
		TargetKind:    db.MoveTargetNew,
		TargetOfferID: offer.ProviderID,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if intent.SourceAttemptID == nil || *intent.SourceAttemptID != sourceAttemptID {
		t.Fatalf("source_attempt_id = %v, want %d", intent.SourceAttemptID, sourceAttemptID)
	}

	dst, err := LaunchInstance(
		mockClient, nil, database, nil, group, offer,
		LaunchOpts{MoveTargetClaim: true},
		cloud.R2Config{Bucket: "test", AccountID: "test"},
		cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"},
		R2Assets{Client: &r2.Client{}},
		nil,
		func(string) {},
		nil,
	)
	// MoveTargetClaim must succeed at the claim step, so the error returned
	// is the CreateInstance failure (proving control reached CreateInstance,
	// i.e. past the claim). Without MoveTargetClaim, the same setup yields
	// "all jobs claimed by other launches" before CreateInstance is called.
	if err == nil || !errors.Is(err, stopAfterRegistration) {
		t.Fatalf("LaunchInstance with MoveTargetClaim=true: err = %v, want stop-after-registration", err)
	}
	if dst == 0 || dst == src {
		t.Fatalf("dst id = %d (src = %d)", dst, src)
	}

	reloaded, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID after failed move launch: %v", err)
	}
	if reloaded.LaunchID == nil || *reloaded.LaunchID != src {
		t.Fatalf("source launch after failed move launch = %v, want %d", reloaded.LaunchID, src)
	}
	gotIntent, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent after failed move launch: %v", err)
	}
	if gotIntent.State != db.MoveIntentStateOpen {
		t.Fatalf("move intent state = %q, want open", gotIntent.State)
	}
	// No cancel-attempts marker should be sent until the target has accepted
	// the job. This launch failed before provider create succeeded.
	if got := canceledByLaunch[src]; len(got) != 0 {
		t.Errorf("cancel-attempts to source = %v, want none before target acceptance", canceledByLaunch)
	}
}

func TestAwaitAllPerDir_PartialSuccess(t *testing.T) {
	stager := &R2AssetStager{
		Client:         &r2.Client{},
		AgentVersion:   "test",
		agentKey:       newStringPromise(),
		sourcePromises: map[string]*sourceUploadPromise{},
	}
	defer stager.Close()
	stager.agentKey.resolve("agent-r2-key", nil)
	ok1 := newSourceUploadPromise()
	ok1.resolve(weftsync.SourceUploadResult{Manifest: weftsync.SourceManifest{Roots: []weftsync.SourceRoot{{LocalPath: "/projects/good-1", R2Key: "source-key-1"}}}}, nil)
	ok2 := newSourceUploadPromise()
	ok2.resolve(weftsync.SourceUploadResult{Manifest: weftsync.SourceManifest{Roots: []weftsync.SourceRoot{{LocalPath: "/projects/good-2", R2Key: "source-key-2"}}}}, nil)
	bad := newSourceUploadPromise()
	bad.resolve(weftsync.SourceUploadResult{}, errors.New("source directory exceeds 500 MB limit"))
	stager.sourcePromises["/projects/good-1"] = ok1
	stager.sourcePromises["/projects/good-2"] = ok2
	stager.sourcePromises["/projects/too-big"] = bad

	assets, perDirErr, err := stager.AwaitAllPerDir()
	if err != nil {
		t.Fatalf("AwaitAllPerDir returned fatal error: %v", err)
	}
	if assets == nil {
		t.Fatal("assets should not be nil when agent upload succeeded")
	}
	if assets.AgentR2Key != "agent-r2-key" {
		t.Fatalf("AgentR2Key = %q, want %q", assets.AgentR2Key, "agent-r2-key")
	}
	if got := assets.SourceR2Keys["/projects/good-1"]; got != "source-key-1" {
		t.Fatalf("good-1 key = %q, want %q", got, "source-key-1")
	}
	if got := assets.SourceR2Keys["/projects/good-2"]; got != "source-key-2" {
		t.Fatalf("good-2 key = %q, want %q", got, "source-key-2")
	}
	if _, present := assets.SourceR2Keys["/projects/too-big"]; present {
		t.Error("too-big key should NOT be in SourceR2Keys")
	}
	if len(perDirErr) != 1 {
		t.Fatalf("perDirErr has %d entries, want 1", len(perDirErr))
	}
	if perDirErr["/projects/too-big"] == nil {
		t.Error("perDirErr should contain /projects/too-big")
	}
}

func TestAwaitAllPerDir_AgentFailureIsFatal(t *testing.T) {
	stager := &R2AssetStager{
		Client:         &r2.Client{},
		AgentVersion:   "test",
		agentKey:       newStringPromise(),
		sourcePromises: map[string]*sourceUploadPromise{},
	}
	defer stager.Close()
	stager.agentKey.resolve("", errors.New("R2 network failure"))
	ok := newSourceUploadPromise()
	ok.resolve(weftsync.SourceUploadResult{Manifest: weftsync.SourceManifest{Roots: []weftsync.SourceRoot{{LocalPath: "/projects/foo", R2Key: "source-key"}}}}, nil)
	stager.sourcePromises["/projects/foo"] = ok

	assets, perDirErr, err := stager.AwaitAllPerDir()
	if err == nil {
		t.Fatal("expected fatal error from agent upload failure")
	}
	if assets != nil {
		t.Errorf("assets = %#v, want nil when agent upload fails", assets)
	}
	if perDirErr != nil {
		t.Errorf("perDirErr = %v, want nil when agent upload fails", perDirErr)
	}
}

func TestGroupSourceUploadError(t *testing.T) {
	group := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 1, WorkingDir: "/projects/alpha"},
			{ID: 2, WorkingDir: "/projects/alpha"},
		},
	}
	if err := groupSourceUploadError(group, nil); err != nil {
		t.Errorf("nil map: got %v, want nil", err)
	}
	if err := groupSourceUploadError(group, map[string]error{"/other/project": errors.New("x")}); err != nil {
		t.Errorf("unrelated dir error: got %v, want nil", err)
	}
	upErr := errors.New("source directory exceeds 500 MB limit")
	got := groupSourceUploadError(group, map[string]error{"/projects/alpha": upErr})
	if got == nil {
		t.Fatal("expected non-nil error for matching dir")
	}
	if !strings.Contains(got.Error(), "/projects/alpha") {
		t.Errorf("error %v should include the failing dir path", got)
	}
	if !errors.Is(got, upErr) {
		t.Errorf("error %v should wrap underlying upload error", got)
	}
}

// TestCancelNonTerminalCampaignInstances guards CampaignFailed's
// "all instances terminal" invariant: every campaign launch must be
// terminal before the campaign itself is stamped failed.
func TestCancelNonTerminalCampaignInstances(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusPlanned})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	plannedID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusPlanned,
		Provider:   "vastai",
		GPUSpec:    "donor",
	})
	if err != nil {
		t.Fatalf("create planned launch: %v", err)
	}
	launchingID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusLaunching,
		Provider:   "vastai",
		GPUSpec:    "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launching launch: %v", err)
	}
	terminalID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusFailed,
		Provider:   "vastai",
		GPUSpec:    "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create terminal launch: %v", err)
	}

	cancelNonTerminalCampaignInstances(database, campaignID, "test detail")

	cases := []struct {
		name string
		id   int64
		want string
	}{
		{"planned -> canceled", plannedID, db.LaunchStatusCancelled},
		{"launching -> canceled", launchingID, db.LaunchStatusCancelled},
		{"failed -> failed (unchanged)", terminalID, db.LaunchStatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.GetLaunch(database, tc.id)
			if err != nil {
				t.Fatalf("GetLaunch: %v", err)
			}
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q", got.Status, tc.want)
			}
			if tc.want == db.LaunchStatusCancelled && got.TerminationDetail != "test detail" {
				t.Errorf("termination_detail = %q, want \"test detail\"", got.TerminationDetail)
			}
		})
	}
}

// Regression test: post-create launch failure paths must reset claimed jobs
// (resetLaunchJobsForFailure), not leave them claimed on a dead launch until
// the periodic repair pass. This exercises the "failed to record provider ID"
// path by aborting the provider_instance_id write with a SQLite trigger.
func TestLaunchInstanceProviderIDRecordFailureResetsJobs(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	var destroyedProviderID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			return &cloud.Instance{
				ProviderID:  "prov-12345",
				Status:      cloud.ProviderStatusRunning,
				CostPerHour: 0.50,
			}, nil
		},
		ShowInstanceFunc: func(string) (*cloud.Instance, error) {
			return nil, errors.New("readback unavailable in test")
		},
		DestroyInstanceFunc: func(instanceID string) error {
			destroyedProviderID = instanceID
			return nil
		},
	}

	job := &db.Job{
		ID:      103,
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

	// Make SetLaunchProviderID fail so the post-create failure path runs.
	if _, err := database.Exec(
		`CREATE TRIGGER test_fail_provider_id_write
		 BEFORE UPDATE OF provider_instance_id ON launches
		 BEGIN SELECT RAISE(ABORT, 'simulated provider-id write failure'); END`,
	); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	instanceID, err := LaunchInstance(
		mockClient, nil, database, nil, group, offer,
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
	if !strings.Contains(err.Error(), "record provider instance ID") {
		t.Fatalf("error = %v, want record-provider-ID failure", err)
	}
	if destroyedProviderID != "prov-12345" {
		t.Errorf("destroyed provider instance = %q, want prov-12345 (leak cleanup)", destroyedProviderID)
	}

	ci, getErr := db.GetLaunch(database, instanceID)
	if getErr != nil {
		t.Fatalf("get cloud instance: %v", getErr)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}

	// The claimed job must be released immediately, not left for the
	// periodic repair pass.
	resetJob, jobErr := db.GetJobByID(database, job.ID)
	if jobErr != nil {
		t.Fatalf("GetJobByID: %v", jobErr)
	}
	if resetJob.LaunchID != nil {
		t.Fatalf("job launch_id = %v, want nil (job should be reset on launch failure)", *resetJob.LaunchID)
	}

	attempts, attErr := db.GetLaunchAttempts(database, job.ID)
	if attErr != nil {
		t.Fatalf("get job attempts: %v", attErr)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d, want 1", len(attempts))
	}
	if attempts[0].Outcome != db.AttemptOutcomeOrphaned {
		t.Fatalf("attempt outcome = %q, want %q", attempts[0].Outcome, db.AttemptOutcomeOrphaned)
	}
}

// Campaign launches carry per-launch weft/i<id> labels, not campaign-scoped
// ones: the orphan sweep resolves the label to the exact owning launch, so
// mid-create instances are recognized and create-retry duplicates reaped
// precisely (see shouldDestroyUnrecordedWeftInstance).
func TestLaunchInstance_LabelsInstancePerLaunchEvenInCampaign(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	var gotLabel string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			gotLabel = opts.Label
			return nil, errors.New("stop after label assignment")
		},
	}

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	job := &db.Job{ID: 101, Status: db.StatusQueued, Command: "python train.py"}
	group := InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24, Jobs: []*db.Job{job}}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, '/tmp', ?, ?, ?, 0)`,
		job.ID, group.GPUClass, group.GPUMemGB, job.Command,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	offer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai}

	launchID, err := LaunchInstance(
		mockClient, nil, database, &campaignID, group, offer,
		LaunchOpts{},
		cloud.R2Config{Bucket: "test", AccountID: "test"},
		cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"},
		R2Assets{Client: &r2.Client{}},
		nil,
		func(string) {},
		nil,
	)
	if err == nil {
		t.Fatal("expected create failure from mock")
	}
	if want := launchProviderLabel(launchID); gotLabel != want {
		t.Fatalf("create label = %q, want %q", gotLabel, want)
	}
}
