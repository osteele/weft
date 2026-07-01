package campaign

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/retrypolicy"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/vastai"
)

// Cloud rental instances are currently always linux/amd64 (Vast.ai, RunPod).
const (
	cloudOS   = "linux"
	cloudArch = "amd64"
)

// LaunchOpts configures an instance launch.
type LaunchOpts struct {
	MaxSpendCents       int
	MaxTimeSeconds      int
	NoDonor             bool                      // skip donor instance strategy
	GracePeriodSeconds  int                       // grace period after job failure (0 = disabled)
	Strategy            bidding.SelectionStrategy // "cheap" (default), "fast", or "fastest"
	ScoreProfile        bidding.ScoreProfile      // optional explicit scoring profile for tradeoff selection
	MinSurvival         float64                   // minimum survival probability; offers below this are skipped (0 = disabled)
	SkipWorkdirDeletion bool                      // disable background workdir cleanup (for debugging)
	GPUWarmup           bool                      // enable GPU warmup before first benchmark job
	DistinctMachines    bool                      // exclude covered/in-flight/avoided physical machines within the campaign
	AvoidMachines       []string                  // resolved machine refs to exclude for distinct-machine campaigns
	AffinityMachines    []string                  // resolved machine refs that eligible offers must match
	// MoveTargetClaim, if true, uses each job's open MoveIntent to create a
	// hidden target attempt on this launch. The source stays authoritative
	// until the target launch is accepted.
	MoveTargetClaim bool

	// HedgeProbe marks this launch as a hedge-cohort probe: jobs are
	// not claimed, and the probe races siblings to agent_ready. See
	// campaign-lifecycle.allium § HedgeCohortCull.
	HedgeProbe bool
	// HedgeCohortID sets launches.hedge_cohort_id at registration.
	// Probes pass the primary's launch id; the primary sets its own
	// id via SetLaunchHedgeCohort after registration.
	HedgeCohortID int64

	// PlacementAlternatives carries ranked cloud-offer alternatives from the
	// planning pass, keyed by LaunchGroupSignature.
	PlacementAlternatives map[string][]RankedOfferAlternative

	distinctAcceptMu         *sync.Mutex
	distinctAcceptedByMach   map[string]int64
	holdRunpodDriverBlockers bool
}

func (opts LaunchOpts) ScoringProfile() bidding.ScoreProfile {
	if opts.ScoreProfile.Valid() {
		return opts.ScoreProfile
	}
	return opts.Strategy.Profile()
}

// ApplyAutoBudget derives budget limits from estimates for any limits not already set.
// Uses the maximum across all group estimates so no instance gets killed prematurely.
// Returns true if any limits were set.
func (opts *LaunchOpts) ApplyAutoBudget(estimates []CostEstimate) bool {
	if opts.MaxSpendCents > 0 && opts.MaxTimeSeconds > 0 {
		return false
	}

	var maxSpend, maxTime int
	for _, est := range estimates {
		spend, secs := BudgetFromEstimate(est)
		if spend > maxSpend {
			maxSpend = spend
		}
		if secs > maxTime {
			maxTime = secs
		}
	}

	changed := false
	if opts.MaxSpendCents == 0 && maxSpend > 0 {
		opts.MaxSpendCents = maxSpend
		changed = true
	}
	if opts.MaxTimeSeconds == 0 && maxTime > 0 {
		opts.MaxTimeSeconds = maxTime
		changed = true
	}
	return changed
}

// LaunchResult holds the outcome of an instance launch.
type LaunchResult struct {
	CampaignID  int64
	InstanceIDs []int64
	Errors      []error
}

type stringPromise struct {
	done chan struct{}
	mu   sync.Mutex
	val  string
	err  error
}

func newStringPromise() *stringPromise {
	return &stringPromise{done: make(chan struct{})}
}

func (p *stringPromise) resolve(val string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return
	default:
		p.val = val
		p.err = err
		close(p.done)
	}
}

func (p *stringPromise) await() (string, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.val, p.err
}

type AssetStageKind string

const (
	AssetStageKindAgent  AssetStageKind = "agent"
	AssetStageKindSource AssetStageKind = "source"
)

// AssetStageStatus describes the current coarse-grained state of one staged
// asset. Ready indicates that the final R2 key is available.
type AssetStageStatus struct {
	Key   string
	Kind  AssetStageKind
	Label string
	Phase string
	Ready bool
	Err   error
}

type AssetStageReporter func(AssetStageStatus)

// R2AssetStager uploads shared campaign assets in the background so group
// launches can start as soon as their own dependencies are ready.
type R2AssetStager struct {
	Client       *r2.Client
	AgentVersion string

	cancel         context.CancelFunc
	agentKey       *stringPromise
	sourcePromises map[string]*stringPromise
	reporter       AssetStageReporter

	statusMu sync.RWMutex
	statuses map[string]AssetStageStatus
}

func (s *R2AssetStager) setStatus(status AssetStageStatus) {
	if s == nil {
		return
	}
	s.statusMu.Lock()
	if s.statuses == nil {
		s.statuses = make(map[string]AssetStageStatus)
	}
	s.statuses[status.Key] = status
	reporter := s.reporter
	s.statusMu.Unlock()
	logAssetStageStatus(status)
	if reporter != nil {
		reporter(status)
	}
}

func assetStageDetail(status AssetStageStatus) string {
	parts := []string{
		fmt.Sprintf("kind=%s", status.Kind),
		fmt.Sprintf("phase=%s", status.Phase),
	}
	if status.Label != "" {
		parts = append(parts, fmt.Sprintf("label=%s", status.Label))
	}
	if status.Key != "" && status.Key != status.Label {
		parts = append(parts, fmt.Sprintf("key=%s", status.Key))
	}
	if status.Ready {
		parts = append(parts, "ready=true")
	}
	return strings.Join(parts, " ")
}

func logAssetStageStatus(status AssetStageStatus) {
	op := oplog.OpR2UploadSource
	if status.Kind == AssetStageKindAgent {
		op = oplog.OpR2UploadAgent
	}
	opts := []oplog.Option{oplog.WithDetail(assetStageDetail(status))}
	if status.Err != nil {
		opts = append(opts, oplog.WithError(status.Err))
	}
	oplog.Log(op, opts...)
}

// SnapshotStatuses returns a shallow copy of current asset statuses keyed by
// asset key ("agent" or local source dir).
func (s *R2AssetStager) SnapshotStatuses() map[string]AssetStageStatus {
	if s == nil {
		return nil
	}
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	out := make(map[string]AssetStageStatus, len(s.statuses))
	for key, status := range s.statuses {
		out[key] = status
	}
	return out
}

func (s *R2AssetStager) AssetCounts() (ready, total int) {
	if s == nil {
		return 0, 0
	}
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	total = len(s.statuses)
	for _, status := range s.statuses {
		if status.Ready {
			ready++
		}
	}
	return ready, total
}

// StartR2AssetStaging starts uploading the agent binary and source tarballs to
// R2 in the background. Group launches can wait only on the directories they
// need instead of blocking on all assets globally.
func StartR2AssetStaging(r2Cfg cloud.R2Config, groups []InstanceGroup) (*R2AssetStager, error) {
	return StartR2AssetStagingWithReporter(r2Cfg, groups, nil)
}

// StartR2AssetStagingWithReporter starts uploading shared assets and reports
// coarse asset phase changes via reporter.
func StartR2AssetStagingWithReporter(r2Cfg cloud.R2Config, groups []InstanceGroup, reporter AssetStageReporter) (*R2AssetStager, error) {
	agentVersion, err := agentdeploy.LocalAgentVersion()
	if err != nil {
		return nil, fmt.Errorf("local agent version: %w", err)
	}

	r2Client, err := r2.New(r2.Config{
		AccountID:       r2Cfg.AccountID,
		AccessKeyID:     r2Cfg.AccessKeyID,
		SecretAccessKey: r2Cfg.SecretAccessKey,
		Bucket:          r2Cfg.Bucket,
	})
	if err != nil {
		return nil, fmt.Errorf("create R2 client: %w", err)
	}

	uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 10*time.Minute)

	stager := &R2AssetStager{
		Client:         r2Client,
		AgentVersion:   agentVersion,
		cancel:         uploadCancel,
		agentKey:       newStringPromise(),
		sourcePromises: make(map[string]*stringPromise),
		reporter:       reporter,
		statuses:       make(map[string]AssetStageStatus),
	}
	stager.setStatus(AssetStageStatus{
		Key:   "agent",
		Kind:  AssetStageKindAgent,
		Label: "agent",
		Phase: "queued",
	})

	allSourceDirs := make(map[string]bool)
	sourceInputsByDir := make(map[string][]string)
	for _, g := range groups {
		for _, d := range g.SourceDirs() {
			allSourceDirs[d] = true
		}
		for d, inputs := range SourceInputsByDir(g.Jobs) {
			sourceInputsByDir[d] = mergeStringSlices(sourceInputsByDir[d], inputs)
		}
	}
	for localDir := range allSourceDirs {
		stager.sourcePromises[localDir] = newStringPromise()
		stager.setStatus(AssetStageStatus{
			Key:   localDir,
			Kind:  AssetStageKindSource,
			Label: filepath.Base(localDir),
			Phase: "queued",
		})
	}

	// Log periodic warnings so the user knows the upload is still in progress.
	uploadDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		elapsed := 15 * time.Second
		for {
			select {
			case <-uploadDone:
				return
			case <-ticker.C:
				slog.Debug("R2 asset upload still in progress", "component", "launch", "elapsed", elapsed.Truncate(time.Second))
				elapsed += 15 * time.Second
			}
		}
	}()
	go func() {
		agentR2Key, err := agentdeploy.EnsureAgentInR2WithProgress(uploadCtx, r2Client, agentVersion, cloudOS, cloudArch, io.Discard, func(phase string) {
			stager.setStatus(AssetStageStatus{
				Key:   "agent",
				Kind:  AssetStageKindAgent,
				Label: "agent",
				Phase: phase,
				Ready: phase == "ready",
			})
		})
		if err != nil {
			stager.setStatus(AssetStageStatus{
				Key:   "agent",
				Kind:  AssetStageKindAgent,
				Label: "agent",
				Phase: "error",
				Err:   err,
			})
		}
		stager.agentKey.resolve(agentR2Key, err)
	}()

	var uploadWg sync.WaitGroup
	for localDir, promise := range stager.sourcePromises {
		localDir := localDir
		promise := promise
		inputs := sourceInputsByDir[localDir]
		slog.Debug("source upload: queuing", "component", "launch",
			"localDir", localDir, "inputCount", len(inputs), "inputs", inputs)
		uploadWg.Add(1)
		go func() {
			defer uploadWg.Done()
			key, err := weftsync.UploadSourceToR2WithProgressForInputs(uploadCtx, r2Client, localDir, inputs, func(phase string) {
				stager.setStatus(AssetStageStatus{
					Key:   localDir,
					Kind:  AssetStageKindSource,
					Label: filepath.Base(localDir),
					Phase: phase,
					Ready: phase == "ready",
				})
			})
			if err != nil {
				oplog.Log(oplog.OpR2UploadSource,
					oplog.WithDetailf("dir: %s", localDir),
					oplog.WithError(err),
				)
				stager.setStatus(AssetStageStatus{
					Key:   localDir,
					Kind:  AssetStageKindSource,
					Label: filepath.Base(localDir),
					Phase: "error",
					Err:   err,
				})
				promise.resolve("", fmt.Errorf("upload source %s: %w", localDir, err))
				return
			}
			promise.resolve(key, nil)
		}()
	}

	go func() {
		_, _ = stager.agentKey.await()
		uploadWg.Wait()
		close(uploadDone)
	}()

	return stager, nil
}

func (s *R2AssetStager) Close() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

// AwaitAssetsForDirs waits for the agent binary and source tarballs needed by
// the given dirs. If onProgress is non-nil, it is called with (completed, total)
// counts starting at (0, total) and after each asset resolves.
func (s *R2AssetStager) AwaitAssetsForDirs(dirs []string, onProgress func(done, total int)) (R2Assets, error) {
	if s == nil || s.Client == nil {
		return R2Assets{}, fmt.Errorf("R2 asset stager is not initialized")
	}
	if onProgress == nil {
		onProgress = func(int, int) {}
	}
	total := 1 + len(dirs) // agent + source dirs
	done := 0
	onProgress(done, total)

	agentR2Key, err := s.agentKey.await()
	if err != nil {
		return R2Assets{}, fmt.Errorf("upload agent to R2: %w", err)
	}
	done++
	onProgress(done, total)

	assets := R2Assets{
		Client:       s.Client,
		AgentVersion: s.AgentVersion,
		AgentR2Key:   agentR2Key,
		SourceR2Keys: make(map[string]string),
	}
	for _, localDir := range dirs {
		promise, ok := s.sourcePromises[localDir]
		if !ok {
			continue
		}
		key, err := promise.await()
		if err != nil {
			return R2Assets{}, err
		}
		assets.SourceR2Keys[localDir] = key
		done++
		onProgress(done, total)
	}
	return assets, nil
}

func (s *R2AssetStager) AwaitAll() (*R2Assets, error) {
	if s == nil {
		return nil, fmt.Errorf("R2 asset stager is not initialized")
	}
	dirs := make([]string, 0, len(s.sourcePromises))
	for localDir := range s.sourcePromises {
		dirs = append(dirs, localDir)
	}
	assets, err := s.AwaitAssetsForDirs(dirs, nil)
	if err != nil {
		return nil, err
	}
	return &assets, nil
}

// AwaitAllPerDir is the partial-success variant of AwaitAll. The agent
// upload is still treated as fatal — without an agent no group can launch.
// Per-source-dir upload failures are returned in perDirErr instead of
// aborting the wait, so callers can isolate the failed groups and proceed
// with the rest. SourceR2Keys in the returned assets is populated only for
// dirs whose upload succeeded.
func (s *R2AssetStager) AwaitAllPerDir() (*R2Assets, map[string]error, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("R2 asset stager is not initialized")
	}
	agentR2Key, err := s.agentKey.await()
	if err != nil {
		return nil, nil, fmt.Errorf("upload agent to R2: %w", err)
	}
	assets := &R2Assets{
		Client:       s.Client,
		AgentVersion: s.AgentVersion,
		AgentR2Key:   agentR2Key,
		SourceR2Keys: make(map[string]string),
	}
	var perDirErr map[string]error
	for localDir, promise := range s.sourcePromises {
		key, err := promise.await()
		if err != nil {
			if perDirErr == nil {
				perDirErr = make(map[string]error)
			}
			perDirErr[localDir] = err
			continue
		}
		assets.SourceR2Keys[localDir] = key
	}
	return assets, perDirErr, nil
}

// PrepareR2Assets uploads the agent binary and source tarballs to R2,
// returning the pre-staged assets for use by LaunchInstance. This is
// extracted from LaunchCampaign so that RelaunchOrphanedJobs can reuse it.
func PrepareR2Assets(r2Cfg cloud.R2Config, groups []InstanceGroup) (*R2Assets, error) {
	return PrepareR2AssetsWithReporter(r2Cfg, groups, nil)
}

func PrepareR2AssetsWithReporter(r2Cfg cloud.R2Config, groups []InstanceGroup, reporter AssetStageReporter) (*R2Assets, error) {
	stager, err := StartR2AssetStagingWithReporter(r2Cfg, groups, reporter)
	if err != nil {
		return nil, err
	}
	defer stager.Close()
	return stager.AwaitAll()
}

// PrepareR2AssetsPerDir is the partial-success variant of PrepareR2Assets.
// It returns whatever assets it was able to stage plus a map of per-dir
// upload errors. The outer error is reserved for failures that prevent any
// launch (agent upload, stager init). See AwaitAllPerDir.
func PrepareR2AssetsPerDir(r2Cfg cloud.R2Config, groups []InstanceGroup) (*R2Assets, map[string]error, error) {
	return PrepareR2AssetsPerDirWithReporter(r2Cfg, groups, nil)
}

func PrepareR2AssetsPerDirWithReporter(r2Cfg cloud.R2Config, groups []InstanceGroup, reporter AssetStageReporter) (*R2Assets, map[string]error, error) {
	stager, err := StartR2AssetStagingWithReporter(r2Cfg, groups, reporter)
	if err != nil {
		return nil, nil, err
	}
	defer stager.Close()
	return stager.AwaitAllPerDir()
}

type replacementOfferFunc func(excludeOfferKeys map[string]struct{}) (*cloud.Offer, error)

func launchCostInputs(estimates []CostEstimate, idx int) (jobDurationHrs, setupOverheadHrs float64) {
	jobDurationHrs = 1.0
	setupOverheadHrs = 0.5
	if idx < 0 || idx >= len(estimates) {
		return jobDurationHrs, setupOverheadHrs
	}
	if runHours := estimates[idx].Breakdown.Run.Mean.Hours(); runHours > 0 {
		jobDurationHrs = runHours
	}
	if setupHours := estimates[idx].SetupOverhead.Hours(); setupHours > 0 {
		setupOverheadHrs = setupHours
	}
	return jobDurationHrs, setupOverheadHrs
}

// runpodSSHBootstrapTimeout must stay below launchingPhaseTimeout so the
// launch goroutine fails before the reconciler's launching-phase safety net.
const runpodSSHBootstrapTimeout = 10 * time.Minute

const (
	runpodSSHWaitingPhase   = "waiting for RunPod SSH readiness"
	runpodSSHReadyPhase     = "RunPod SSH ready; starting bootstrap"
	runpodSSHBootstrapPhase = "running bootstrap over RunPod SSH"
	runpodSSHFinishedPhase  = "RunPod SSH bootstrap finished"
)

// isRetryableCreateError returns true if the error warrants trying a different offer.
func isRetryableCreateError(err error) bool {
	return errors.Is(err, cloud.ErrOfferUnavailable) ||
		errors.Is(err, cloud.ErrProviderRejected) ||
		errors.Is(err, cloud.ErrProviderCommandTimeout)
}

type distinctMachinePostCreateConflictError struct {
	LaunchID   int64
	Provider   cloud.Provider
	ProviderID string
	MachineKey string
	Reason     string
	KeepAlive  bool
}

func (e *distinctMachinePostCreateConflictError) Error() string {
	if e == nil {
		return ""
	}
	if e.Reason != "" {
		return e.Reason
	}
	if e.MachineKey != "" {
		return fmt.Sprintf("sampled duplicate machine %s", e.MachineKey)
	}
	return "sampled machine has no provider identity"
}

func (e *distinctMachinePostCreateConflictError) Unwrap() error {
	return cloud.ErrOfferUnavailable
}

const phaseDriverTooOld = "infra-failure:driver-too-old"

type postCreateDriverCompatibilityError struct {
	LaunchID      int64
	Provider      cloud.Provider
	ProviderID    string
	MachineKey    string
	RequiredMajor int
	ActualMajor   int
	ActualVersion string
	KeepAlive     bool
}

func (e *postCreateDriverCompatibilityError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("RunPod pod driver %s (major %d) is below required major %d",
		e.ActualVersion, e.ActualMajor, e.RequiredMajor)
}

func (e *postCreateDriverCompatibilityError) Unwrap() error {
	return cloud.ErrOfferUnavailable
}

type postCreateDriverFailureReport struct {
	TimestampUnix   int64  `json:"timestamp_unix"`
	RequiredMajor   int    `json:"required_driver_major"`
	ActualMajor     int    `json:"actual_driver_major"`
	ActualVersion   string `json:"actual_driver_version"`
	NvidiaSmiOutput string `json:"nvidia_smi_query_output,omitempty"`
}

type runpodDriverVersionProber interface {
	ProbeDriverVersion(context.Context, string) (string, error)
}

func runpodObservedBootstrapPhase(phase string) string {
	switch strings.TrimSpace(phase) {
	case "waiting for SSH":
		return runpodSSHWaitingPhase
	case "SSH ready":
		return runpodSSHReadyPhase
	case "running bootstrap script":
		return runpodSSHBootstrapPhase
	case "bootstrap script finished":
		return runpodSSHFinishedPhase
	default:
		return ""
	}
}

func runpodPostCreateDriverCompatibility(
	ctx context.Context,
	r2Client *r2.Client,
	client cloud.Client,
	instanceID int64,
	providerInstID string,
	machineID string,
	requiredMajor int,
	keepAlive bool,
	progress cloud.ProgressFunc,
) error {
	if requiredMajor <= 0 || client == nil || client.Provider() != cloud.ProviderRunpod {
		return nil
	}
	prober, ok := client.(runpodDriverVersionProber)
	if !ok {
		slog.Debug("runpod driver probe unsupported", "launch_id", instanceID)
		return nil
	}
	if progress != nil {
		progress("probing RunPod driver compatibility")
	}
	probeCtx, cancel := context.WithTimeout(ctx, runpodSSHBootstrapTimeout)
	defer cancel()
	out, err := prober.ProbeDriverVersion(probeCtx, providerInstID)
	if err != nil {
		slog.Warn("runpod driver probe failed; deferring to agent preflight",
			"component", "launch", "launch_id", instanceID, "provider_id", providerInstID, "error", err)
		return nil
	}
	version, major, ok := parseDriverMajor(out)
	if !ok {
		slog.Warn("runpod driver probe returned unparseable output; deferring to agent preflight",
			"component", "launch", "launch_id", instanceID, "provider_id", providerInstID, "output", strings.TrimSpace(out))
		return nil
	}
	if major >= requiredMajor {
		if progress != nil {
			progress(fmt.Sprintf("RunPod driver compatible: %s", version))
		}
		return nil
	}

	report := postCreateDriverFailureReport{
		TimestampUnix:   time.Now().Unix(),
		RequiredMajor:   requiredMajor,
		ActualMajor:     major,
		ActualVersion:   version,
		NvidiaSmiOutput: out,
	}
	if r2Client != nil && r2Client.IsConfigured() {
		if data, marshalErr := json.MarshalIndent(report, "", "  "); marshalErr == nil {
			_ = r2Client.PutObject(ctx, r2keys.InstanceDriverFailure(instanceID), bytes.NewReader(data), "application/json")
		}
		_ = r2Client.PutObject(ctx, r2keys.InstancePhase(instanceID), strings.NewReader(phaseDriverTooOld), "text/plain")
	}
	if progress != nil {
		progress(fmt.Sprintf("RunPod driver rejected: %s < %d", version, requiredMajor))
	}
	return &postCreateDriverCompatibilityError{
		LaunchID:      instanceID,
		Provider:      client.Provider(),
		ProviderID:    providerInstID,
		MachineKey:    db.ProviderMachineKey(string(cloud.ProviderRunpod), machineID),
		RequiredMajor: requiredMajor,
		ActualMajor:   major,
		ActualVersion: version,
		KeepAlive:     keepAlive && strings.TrimSpace(providerInstID) != "",
	}
}

var driverMajorPattern = regexp.MustCompile(`(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

func parseDriverMajor(out string) (version string, major int, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := driverMajorPattern.FindString(line)
		if m == "" {
			continue
		}
		n, err := strconv.Atoi(strings.SplitN(m, ".", 2)[0])
		if err != nil {
			continue
		}
		return line, n, true
	}
	return "", 0, false
}

func recordRunpodObservedBootstrapPhase(database *sql.DB, campaignID *int64, launchID int64, phase string) {
	observed := runpodObservedBootstrapPhase(phase)
	if observed == "" {
		return
	}
	if _, err := db.SetLaunchLiveInstancePhase(database, launchID, observed); err != nil {
		slog.Debug("record runpod bootstrap phase", "component", "launch", "launch_id", launchID, "phase", observed, "error", err)
	}
	if observed != runpodSSHWaitingPhase {
		return
	}
	event := &db.LifecycleEvent{
		EventKind: db.EventLaunchRunpodSSHWaiting,
		LaunchID:  launchID,
		Detail:    observed,
	}
	if campaignID != nil {
		event.CampaignID = *campaignID
	}
	if err := db.InsertLifecycleEvent(database, event); err != nil {
		slog.Debug("record runpod ssh waiting event", "component", "launch", "launch_id", launchID, "error", err)
	}
}

func validateDistinctMachineOffers(offers []cloud.Offer) error {
	for _, offer := range offers {
		switch offer.Provider {
		case cloud.ProviderVastai:
			if strings.TrimSpace(offer.MachineID) == "" {
				return fmt.Errorf("distinct-machine launches require provider machine_id; offer %s has none", offer.Key())
			}
		case cloud.ProviderRunpod:
			// RunPod exposes machine_id only after pod creation; LaunchInstance
			// performs a late acceptance check before bootstrapping the job.
		default:
			return fmt.Errorf("distinct-machine launches require provider machine identity support; provider %q is unsupported", offer.Provider)
		}
	}
	return nil
}

func MachineIDClients(clients []cloud.Client, feature string) ([]cloud.Client, error) {
	filtered := make([]cloud.Client, 0, len(clients))
	for _, client := range clients {
		if client != nil && client.Provider() == cloud.ProviderVastai {
			filtered = append(filtered, client)
		}
	}
	if len(filtered) == 0 {
		if feature == "" {
			feature = "machine-constrained launches"
		}
		return nil, fmt.Errorf("%s require Vast.ai machine_id support; no Vast.ai provider is available", feature)
	}
	return filtered, nil
}

func DistinctMachineClients(clients []cloud.Client) ([]cloud.Client, error) {
	filtered := make([]cloud.Client, 0, len(clients))
	for _, client := range clients {
		if client == nil {
			continue
		}
		switch client.Provider() {
		case cloud.ProviderVastai, cloud.ProviderRunpod:
			filtered = append(filtered, client)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("distinct-machine launches require Vast.ai or RunPod machine identity support; no supported provider is available")
	}
	return filtered, nil
}

func MachineRefKeys(machineRefs []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, ref := range machineRefs {
		provider, machineID := parseMachineRef(ref)
		if key := db.ProviderMachineKey(provider, machineID); key != "" {
			out[key] = struct{}{}
		}
	}
	return out
}

func VastAIMachineKeys(machineIDs []string) map[string]struct{} {
	return MachineRefKeys(machineIDs)
}

func DistinctMachineAvoidanceKeys(avoidMachines []string) map[string]struct{} {
	return MachineRefKeys(avoidMachines)
}

func distinctMachineExclusions(database *sql.DB, campaignID int64, avoidMachines []string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	covered, err := db.CampaignCoveredMachineIDs(database, campaignID)
	if err != nil {
		return nil, err
	}
	for key := range covered {
		out[key] = struct{}{}
	}
	inflight, err := db.CampaignInflightMachineIDs(database, campaignID)
	if err != nil {
		return nil, err
	}
	for key := range inflight {
		out[key] = struct{}{}
	}
	for key := range DistinctMachineAvoidanceKeys(avoidMachines) {
		out[key] = struct{}{}
	}
	return out, nil
}

func parseMachineRef(ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}
	provider, machineID, ok := strings.Cut(ref, "/")
	if ok {
		return strings.TrimSpace(provider), strings.TrimSpace(machineID)
	}
	return string(cloud.ProviderVastai), ref
}

func runpodDistinctMachineConflict(database *sql.DB, campaignID *int64, currentLaunchID int64, machineID string, avoidMachines []string, acceptedByMachine map[string]int64) (string, error) {
	key := db.ProviderMachineKey(string(cloud.ProviderRunpod), machineID)
	if key == "" {
		return "RunPod pod did not report a machine_id for distinct-machine launch", nil
	}
	if _, avoided := DistinctMachineAvoidanceKeys(avoidMachines)[key]; avoided {
		return fmt.Sprintf("RunPod pod sampled avoided machine %s", key), nil
	}
	if acceptedByMachine != nil {
		if priorID, ok := acceptedByMachine[key]; ok && priorID != currentLaunchID {
			return fmt.Sprintf("RunPod pod sampled machine %s already accepted by %s", key, ids.FormatInstanceID(priorID)), nil
		}
	}
	if campaignID == nil || *campaignID <= 0 {
		if acceptedByMachine != nil {
			acceptedByMachine[key] = currentLaunchID
		}
		return "", nil
	}
	covered, err := db.CampaignCoveredMachineIDs(database, *campaignID)
	if err != nil {
		return "", err
	}
	if _, ok := covered[key]; ok {
		return fmt.Sprintf("RunPod pod sampled already-covered machine %s", key), nil
	}
	var priorID int64
	err = database.QueryRow(`
		SELECT id
		  FROM launches
		 WHERE campaign_id = ?
		   AND id < ?
		   AND COALESCE(provider, '') = ?
		   AND COALESCE(machine_id, '') = ?
		   AND status NOT IN (?, ?, ?)
		 ORDER BY id
		 LIMIT 1`,
		*campaignID,
		currentLaunchID,
		string(cloud.ProviderRunpod),
		strings.TrimSpace(machineID),
		db.LaunchStatusCompleted,
		db.LaunchStatusFailed,
		db.LaunchStatusCancelled,
	).Scan(&priorID)
	if err == sql.ErrNoRows {
		if acceptedByMachine != nil {
			acceptedByMachine[key] = currentLaunchID
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if priorID > 0 {
		return fmt.Sprintf("RunPod pod sampled machine %s already held by %s", key, ids.FormatInstanceID(priorID)), nil
	}
	if acceptedByMachine != nil {
		acceptedByMachine[key] = currentLaunchID
	}
	return "", nil
}

var (
	retryAttemptPattern = regexp.MustCompile(`attempt\s+(\d+)/(\d+)`)
	replanChainPattern  = regexp.MustCompile(`chain\s+(\d+)/(\d+)`)
)

func classifyLaunchGroupPhaseEvent(group InstanceGroup, phase string) LaunchEvent {
	event := LaunchEvent{
		Kind:  LaunchEventGroupPhase,
		Group: group,
		Phase: phase,
	}
	switch {
	case strings.Contains(phase, "replanning with fresh offer"):
		matches := replanChainPattern.FindStringSubmatch(phase)
		if len(matches) != 3 {
			return event
		}
		attempt, errA := strconv.Atoi(matches[1])
		maxAttempts, errM := strconv.Atoi(matches[2])
		if errA != nil || errM != nil {
			return event
		}
		event.Kind = LaunchEventGroupReplan
		event.RetryAttempt = attempt
		event.RetryMax = maxAttempts
		return event
	case strings.Contains(phase, "retrying with replacement offer"):
		matches := retryAttemptPattern.FindStringSubmatch(phase)
		if len(matches) != 3 {
			return event
		}
		attempt, errA := strconv.Atoi(matches[1])
		maxAttempts, errM := strconv.Atoi(matches[2])
		if errA != nil || errM != nil {
			return event
		}
		event.Kind = LaunchEventGroupRetry
		event.RetryAttempt = attempt
		event.RetryMax = maxAttempts
		return event
	}
	return event
}

func createInstanceWithReplacement(
	initialClient cloud.Client,
	clientForProvider func(cloud.Provider) cloud.Client,
	group InstanceGroup,
	offer cloud.Offer,
	createOpts cloud.CreateOpts,
	progress cloud.ProgressFunc,
	updateOfferMetadata func(cloud.Offer) error,
	replacementOffer replacementOfferFunc,
) (*cloud.Instance, cloud.Offer, cloud.Client, error) {
	client := initialClient
	currentOffer := offer
	excludedOffers := make(map[string]struct{})
	var lastErr error
	maxAttempts := retrypolicy.MaxCreateAttempts()

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt == 1 {
			progress("creating instance")
		} else {
			progress(fmt.Sprintf("retrying with replacement offer (attempt %d/%d)", attempt, maxAttempts))
		}

		var (
			inst *cloud.Instance
			err  error
		)
		createAttemptOpts := createOpts
		if currentOffer.NumGPUs > 0 {
			createAttemptOpts.GPUCount = currentOffer.NumGPUs
		}
		if createAttemptOpts.InstanceType == cloud.InstanceTypeInterruptible && createAttemptOpts.MaxBidPrice <= 0 {
			createAttemptOpts.MaxBidPrice = currentOffer.CostPerHour
		}
		if progressClient, ok := client.(cloud.ProgressClient); ok {
			inst, err = progressClient.CreateInstanceWithProgress(currentOffer.ProviderID, createAttemptOpts, progress)
		} else {
			inst, err = client.CreateInstance(currentOffer.ProviderID, createAttemptOpts)
		}
		if err == nil {
			return inst, currentOffer, client, nil
		}
		lastErr = err

		if replacementOffer == nil || !isRetryableCreateError(err) || attempt == maxAttempts {
			break
		}

		excludedOffers[currentOffer.Key()] = struct{}{}

		slog.Warn("instance creation failed, searching for replacement",
			"component", "launch",
			"attempt", attempt,
			"offer", currentOffer.ProviderID,
			"provider", client.Provider(),
			"gpu_spec", group.GPUSpec(),
			"error", err,
		)
		progress(fmt.Sprintf("offer %s failed; searching again", currentOffer.ProviderID))

		nextOffer, retryErr := replacementOffer(excludedOffers)
		if retryErr != nil {
			return nil, currentOffer, client, fmt.Errorf("search replacement offer: %w", retryErr)
		}
		if nextOffer == nil {
			return nil, currentOffer, client, fmt.Errorf("offer %s failed, no replacement found: %w", currentOffer.ProviderID, ErrNoReplacementOffer)
		}

		// When the replacement offer comes from a different cloud provider,
		// swap the active client so the subsequent CreateInstance call (and
		// any downstream ShowInstance / DestroyInstance routing the caller
		// runs after we return) targets the right provider. Without this
		// swap the loop would call e.g. RunPod's CreateInstance with a
		// Vast.ai offer ID, which fails immediately and burns an attempt.
		if nextOffer.Provider != "" && nextOffer.Provider != client.Provider() {
			if clientForProvider == nil {
				return nil, currentOffer, client, fmt.Errorf(
					"replacement offer is from provider %q but no cross-provider client lookup was supplied", nextOffer.Provider)
			}
			swapped := clientForProvider(nextOffer.Provider)
			if swapped == nil {
				return nil, currentOffer, client, fmt.Errorf(
					"replacement offer is from provider %q but no client is configured for it", nextOffer.Provider)
			}
			client = swapped
		}

		if updateOfferMetadata != nil {
			if err := updateOfferMetadata(*nextOffer); err != nil {
				return nil, currentOffer, client, fmt.Errorf("update replacement offer metadata: %w", err)
			}
		}

		currentOffer = *nextOffer
		slog.Info("retrying with replacement offer",
			"component", "launch",
			"attempt", attempt+1,
			"gpu_spec", group.GPUSpec(),
			"offer", currentOffer.ProviderID,
			"provider", client.Provider(),
		)
	}

	return nil, currentOffer, client, lastErr
}

// LaunchCampaign creates a campaign record and launches instances for each group
// in parallel. It collects results and updates the campaign status.
// The onEvent callback, if non-nil, is called with progress updates for campaign
// lifecycle steps and per-group launch activity.
func LaunchCampaign(
	clients []cloud.Client,
	database *sql.DB,
	groups []InstanceGroup,
	offers []cloud.Offer, // parallel to groups
	estimates []CostEstimate, // parallel to groups; may be nil
	survivalModel *bidding.SurvivalModel,
	opts LaunchOpts,
	r2Cfg cloud.R2Config,
	createOptsForProvider func(cloud.Provider) (cloud.CreateOpts, error),
	onEvent func(event LaunchEvent),
	onCampaignCreated func(id int64), // called after campaign record is created, before instances launch; may be nil
	onInstanceRegistered func(group InstanceGroup, instanceID int64),
) (*LaunchResult, error) {
	return launchCampaignWithStager(
		nil,
		clients,
		database,
		groups,
		offers,
		estimates,
		survivalModel,
		opts,
		r2Cfg,
		createOptsForProvider,
		onEvent,
		onCampaignCreated,
		onInstanceRegistered,
	)
}

// LaunchCampaignWithAssetStager reuses a caller-provided asset stager so
// uploads can begin before launch confirmation.
func LaunchCampaignWithAssetStager(
	stager *R2AssetStager,
	clients []cloud.Client,
	database *sql.DB,
	groups []InstanceGroup,
	offers []cloud.Offer,
	estimates []CostEstimate,
	survivalModel *bidding.SurvivalModel,
	opts LaunchOpts,
	r2Cfg cloud.R2Config,
	createOptsForProvider func(cloud.Provider) (cloud.CreateOpts, error),
	onEvent func(event LaunchEvent),
	onCampaignCreated func(id int64),
	onInstanceRegistered func(group InstanceGroup, instanceID int64),
) (*LaunchResult, error) {
	return launchCampaignWithStager(
		stager,
		clients,
		database,
		groups,
		offers,
		estimates,
		survivalModel,
		opts,
		r2Cfg,
		createOptsForProvider,
		onEvent,
		onCampaignCreated,
		onInstanceRegistered,
	)
}

func launchCampaignWithStager(
	preparedStager *R2AssetStager,
	clients []cloud.Client,
	database *sql.DB,
	groups []InstanceGroup,
	offers []cloud.Offer, // parallel to groups
	estimates []CostEstimate, // parallel to groups; may be nil
	survivalModel *bidding.SurvivalModel,
	opts LaunchOpts,
	r2Cfg cloud.R2Config,
	createOptsForProvider func(cloud.Provider) (cloud.CreateOpts, error),
	onEvent func(event LaunchEvent),
	onCampaignCreated func(id int64), // called after campaign record is created, before instances launch; may be nil
	onInstanceRegistered func(group InstanceGroup, instanceID int64),
) (*LaunchResult, error) {
	if len(groups) == 0 {
		return nil, ErrNoLaunchGroups
	}
	if len(offers) != len(groups) {
		return nil, fmt.Errorf("offers/groups mismatch: %d offers for %d groups", len(offers), len(groups))
	}
	if opts.DistinctMachines {
		if err := validateDistinctMachineOffers(offers); err != nil {
			return nil, err
		}
	}

	if onEvent != nil {
		onEvent(LaunchEvent{
			Kind:  LaunchEventCampaignStatus,
			Group: InstanceGroup{GPUClass: "campaign"},
			Phase: "staging agent and sources",
		})
	}
	stager := preparedStager
	ownedStager := false
	var err error
	if stager == nil {
		stager, err = StartR2AssetStaging(r2Cfg, groups)
		if err != nil {
			return nil, err
		}
		ownedStager = true
	}
	if ownedStager {
		defer stager.Close()
	}

	// Compute total estimated cost from estimates
	var estimatedCostCents int
	if estimates != nil {
		totalCost := TotalEstimatedCostFromEstimates(estimates)
		estimatedCostCents = int(totalCost * 100)
	}

	// Create campaign batch record
	if onEvent != nil {
		onEvent(LaunchEvent{
			Kind:  LaunchEventCampaignStatus,
			Group: InstanceGroup{GPUClass: "campaign"},
			Phase: "creating campaign record",
		})
	}
	campaignRec := &db.Campaign{
		Status:             db.CampaignStatusLaunching,
		EstimatedCostCents: estimatedCostCents,
		DistinctMachines:   opts.DistinctMachines,
		AvoidMachines:      opts.AvoidMachines,
		AffinityMachines:   opts.AffinityMachines,
	}
	campaignID, err := db.CreateCampaign(database, campaignRec)
	if err != nil {
		return nil, fmt.Errorf("create campaign: %w", err)
	}

	if onCampaignCreated != nil {
		onCampaignCreated(campaignID)
	}
	campaignCreatedAt := time.Now()
	if c, err := db.GetCampaign(database, campaignID); err == nil && c != nil && c.CreatedAt > 0 {
		campaignCreatedAt = time.Unix(c.CreatedAt, 0)
	}
	selectedMachineKeys := map[string]struct{}{}
	if opts.DistinctMachines {
		for _, offer := range offers {
			if key := offerMachineClaimKey(offer); key != "" {
				selectedMachineKeys[key] = struct{}{}
			}
		}
		if opts.distinctAcceptMu == nil {
			opts.distinctAcceptMu = &sync.Mutex{}
		}
		if opts.distinctAcceptedByMach == nil {
			opts.distinctAcceptedByMach = map[string]int64{}
		}
	}
	appCfg, cfgErr := config.Load()
	if cfgErr != nil {
		appCfg = config.DefaultConfig()
	}
	donorABRate := appCfg.DonorABSampleRate()
	donorControl := false
	if opts.NoDonor {
		_ = db.RecordDonorExperiment(database, campaignID, "manual_no_donor", 0, nil, "--no-donor")
	} else if appCfg.DonorABEnabled() && db.ShouldSample(fmt.Sprintf("donor:%d", campaignID), donorABRate) {
		donorControl = true
		_ = db.RecordDonorExperiment(database, campaignID, "hub_direct_control", donorABRate, nil, "deterministic donor A/B control")
	}

	// Donor strategy: find a cheap collocated instance for cache seeding
	var donorCfg *DonorConfig
	if !opts.NoDonor && !donorControl && len(groups) >= 2 {
		client := clientForProvider(clients, offers[0].Provider)
		if supportsDonorStrategy(client) {
			if onEvent != nil {
				onEvent(LaunchEvent{
					Kind:  LaunchEventCampaignStatus,
					Group: InstanceGroup{GPUClass: "campaign"},
					Phase: "searching for donor instance",
				})
			}
			donorCfg, err = FindDonorOffer(client, offers, estimates, groups)
			if err != nil {
				slog.Warn("donor offer search failed, proceeding without donor", "component", "donor", "error", err)
			}
		}
	}
	if !opts.NoDonor && !donorControl && donorCfg == nil {
		_ = db.RecordDonorExperiment(database, campaignID, "donor_not_selected", donorABRate, nil, "no cost-effective donor")
	}

	// Launch donor instance if strategy is available
	var donorInstanceID int64
	var donorProviderID string
	var donorClient cloud.Client
	if donorCfg != nil {
		donorClient = clientForProvider(clients, donorCfg.Offer.Provider)
		if donorClient != nil {
			if onEvent != nil {
				onEvent(LaunchEvent{
					Kind:  LaunchEventCampaignStatus,
					Group: InstanceGroup{GPUClass: "donor"},
					Phase: "launching donor instance",
				})
			}

			donorInst := &db.Launch{
				CampaignID:       &campaignID,
				Status:           db.LaunchStatusPlanned,
				Provider:         string(donorClient.Provider()),
				GPUSpec:          "donor",
				GPUClass:         donorCfg.Offer.GPUName,
				MaxSpendCents:    opts.MaxSpendCents,
				MaxTimeSeconds:   opts.MaxTimeSeconds,
				ResolvedGPUName:  donorCfg.Offer.GPUName,
				CostPerHourCents: int(donorCfg.Offer.CostPerHour * 100),
				NumGPUs:          donorCfg.Offer.NumGPUs,
				Reliability:      donorCfg.Offer.Reliability,
				InetDownMbps:     donorCfg.Offer.DownloadBandwidth,
				InetUpMbps:       donorCfg.Offer.UploadBandwidth,
				CUDAVersion:      donorCfg.Offer.CUDAVersion,
				InstanceRole:     "donor",
				MachineID:        donorCfg.Offer.MachineID,
			}
			var donorErr error
			donorInstanceID, donorErr = db.CreateLaunch(database, donorInst)
			if donorErr != nil {
				slog.Warn("failed to create donor DB record", "component", "donor", "error", donorErr)
				donorCfg = nil
			} else {
				_ = db.SetLaunchRole(database, donorInstanceID, "donor")
				_ = db.RecordDonorExperiment(database, campaignID, "donor_enabled", donorABRate, &donorInstanceID, "donor selected")

				abortDonor := func(detail string) {
					_ = db.UpdateLaunchStatus(database, donorInstanceID,
						db.LaunchStatusFailed, db.TerminationReasonInfraFailure, detail)
					oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
						"launch_id=%d reason=infra_failure detail=%s", donorInstanceID, detail))
					donorCfg = nil
				}

				// Generate donor bootstrap script
				if onEvent != nil {
					onEvent(LaunchEvent{
						Kind:  LaunchEventCampaignStatus,
						Group: InstanceGroup{GPUClass: "donor"},
						Phase: "waiting for R2 assets",
					})
				}
				donorAssets, donorAssetErr := stager.AwaitAssetsForDirs(donorCfg.SourceDirs, nil)
				if donorAssetErr != nil {
					slog.Warn("donor staging assets failed", "component", "donor", "error", donorAssetErr)
					abortDonor("donor staging assets failed: " + donorAssetErr.Error())
				}
				if donorCfg == nil {
					goto donorDisabled
				}

				var donorSources []SourceMapping
				for _, localDir := range donorCfg.SourceDirs {
					if r2Key, ok := donorAssets.SourceR2Keys[localDir]; ok {
						donorSources = append(donorSources, SourceMapping{
							R2Key:     r2Key,
							RemoteDir: path.Join(cloud.ProjectRootDir, path.Base(localDir)),
							LocalDir:  localDir,
						})
					}
				}

				donorCreateOpts := cloud.CreateOpts{}
				if createOptsForProvider != nil {
					donorCreateOpts, donorErr = createOptsForProvider(donorCfg.Offer.Provider)
					if donorErr != nil {
						slog.Warn("unsupported donor provider config", "component", "donor", "error", donorErr)
						abortDonor("unsupported donor provider config: " + donorErr.Error())
					}
				}
				if donorCfg == nil {
					goto donorDisabled
				}

				donorBootstrap := GenerateBootstrapScript(BootstrapManifest{
					AgentR2Key:   donorAssets.AgentR2Key,
					Sources:      donorSources,
					Image:        donorCreateOpts.Image,
					DonorMode:    true,
					HFModels:     donorCfg.HFModels,
					HFDatasets:   donorCfg.HFDatasets,
					DonorID:      fmt.Sprintf("%d", donorInstanceID),
					DBInstanceID: donorInstanceID,
				})

				if onEvent != nil {
					onEvent(LaunchEvent{
						Kind:  LaunchEventCampaignStatus,
						Group: InstanceGroup{GPUClass: "donor"},
						Phase: "uploading bootstrap script",
					})
				}
				bootstrapKey := r2keys.BootstrapScript(donorInstanceID)
				donorUploadCtx, donorUploadCancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer donorUploadCancel()
				if uploadErr := donorAssets.Client.PutObject(donorUploadCtx, bootstrapKey, strings.NewReader(donorBootstrap), "text/x-shellscript"); uploadErr != nil {
					slog.Warn("failed to upload donor bootstrap", "component", "donor", "error", uploadErr)
					abortDonor("failed to upload donor bootstrap: " + uploadErr.Error())
				} else {
					// Build env vars and create opts for donor
					if donorCfg != nil {
						donorEnvVars := map[string]string{
							"R2_ACCESS_KEY_ID":     r2Cfg.AccessKeyID,
							"R2_SECRET_ACCESS_KEY": r2Cfg.SecretAccessKey,
							"R2_ENDPOINT":          donorAssets.Client.Endpoint(),
							"R2_BUCKET":            r2Cfg.Bucket,
						}
						if apiKey := vastai.ReadAPIKey(); apiKey != "" {
							donorEnvVars["VASTAI_API_KEY"] = apiKey
						}
						if token := os.Getenv("HF_TOKEN"); token != "" {
							donorEnvVars["HF_TOKEN"] = token
							donorEnvVars["HUGGING_FACE_HUB_TOKEN"] = token
						}
						donorCreateOpts.EnvVars = donorEnvVars
						if donorCreateOpts.Image != "" {
							if err := db.UpdateLaunchDockerImage(database, donorInstanceID, donorCreateOpts.Image); err != nil {
								slog.Warn("failed to persist donor docker image", "launch_id", donorInstanceID, "error", err)
							}
						}
						if err := configureBootstrapCreateOpts(donorClient, &donorCreateOpts, bootstrapKey); err != nil {
							slog.Warn("donor bootstrap config failed", "component", "donor", "error", err)
							abortDonor("donor bootstrap config failed: " + err.Error())
						}
					}
					if donorCfg != nil {
						donorCreateOpts.Label = fmt.Sprintf("weft/c%d", campaignID)
						if onEvent != nil {
							onEvent(LaunchEvent{
								Kind:  LaunchEventCampaignStatus,
								Group: InstanceGroup{GPUClass: "donor"},
								Phase: "creating donor instance",
							})
						}

						inst, createErr := donorClient.CreateInstance(donorCfg.Offer.ProviderID, donorCreateOpts)
						if createErr != nil {
							slog.Warn("failed to create donor instance", "component", "donor", "error", createErr)
							abortDonor("donor instance creation failed: " + createErr.Error())
						} else {
							donorProviderID = inst.ProviderID
							_ = db.SetLaunchProviderID(database, donorInstanceID, donorProviderID)
							_ = db.UpdateLaunchStatus(database, donorInstanceID, db.LaunchStatusRunning)
							if donorCfg.Offer.DataCenter != "" {
								_ = db.SetLaunchDataCenter(database, donorInstanceID, donorCfg.Offer.DataCenter)
							}
							slog.Info("donor instance launched", "component", "donor", "provider_id", donorProviderID, "instance", donorInstanceID, "data_center", donorCfg.DataCenter)
						}
					}
				}
			}
		}
	donorDisabled:
	}

	// Launch worker instances in parallel
	if onEvent != nil {
		onEvent(LaunchEvent{
			Kind:  LaunchEventCampaignStatus,
			Group: InstanceGroup{GPUClass: "campaign"},
			Phase: "launching worker instances",
		})
	}
	var mu sync.Mutex
	var instanceIDs []int64
	var workerProviderIDs []workerInfo
	var launchErrors []error
	var wg sync.WaitGroup
	var firstWorkerRegistered bool
	var firstRegistrationTimedOut bool
	firstRegSurvival, _ := db.ComputeFirstRegistrationSurvival(database, firstRegistrationScopeFromOffers(offers))
	stopFirstRegistrationWatch := make(chan struct{})
	if firstRegSurvival != nil {
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopFirstRegistrationWatch:
					return
				case <-ticker.C:
					mu.Lock()
					if firstWorkerRegistered || firstRegistrationTimedOut {
						mu.Unlock()
						return
					}
					elapsed := time.Since(campaignCreatedAt)
					shouldTimeout := elapsed >= firstRegSurvival.TerminateAfter
					if shouldTimeout {
						firstRegistrationTimedOut = true
					}
					mu.Unlock()
					if shouldTimeout {
						stager.Close()
						return
					}
				}
			}
		}()
	}

	for i, g := range groups {
		offer := offers[i]
		var groupEstimate CostEstimate
		if i < len(estimates) {
			groupEstimate = estimates[i]
		}
		jobDurationHrs, setupOverheadHrs := launchCostInputs(estimates, i)

		wg.Add(1)
		go func(group InstanceGroup, ofr cloud.Offer, estimate CostEstimate, jobDurationHrs, setupOverheadHrs float64) {
			defer wg.Done()

			client := clientForProvider(clients, ofr.Provider)
			if client == nil {
				mu.Lock()
				launchErrors = append(launchErrors, fmt.Errorf("%s: no client for provider %s", group.GPUSpec(), ofr.Provider))
				mu.Unlock()
				return
			}

			var progress cloud.ProgressFunc
			if onEvent != nil {
				progress = func(phase string) {
					mu.Lock()
					onEvent(classifyLaunchGroupPhaseEvent(group, phase))
					mu.Unlock()
				}
			}
			instanceRegistered := func(instanceID int64) {
				mu.Lock()
				firstWorkerRegistered = true
				if onInstanceRegistered != nil {
					onInstanceRegistered(group, instanceID)
				}
				mu.Unlock()
			}

			createOpts := cloud.CreateOpts{}
			if createOptsForProvider != nil {
				var err error
				createOpts, err = createOptsForProvider(ofr.Provider)
				if err != nil {
					mu.Lock()
					launchErrors = append(launchErrors, fmt.Errorf("%s: %w", group.GPUSpec(), err))
					mu.Unlock()
					return
				}
			}

			if onEvent != nil {
				mu.Lock()
				onEvent(LaunchEvent{
					Kind:  LaunchEventGroupPhase,
					Group: group,
					Phase: "staging",
				})
				mu.Unlock()
			}
			groupAssets, assetErr := stager.AwaitAssetsForDirs(group.SourceDirs(), func(done, total int) {
				if onEvent == nil {
					return
				}
				mu.Lock()
				onEvent(LaunchEvent{
					Kind:        LaunchEventGroupAssets,
					Group:       group,
					Phase:       "staging",
					AssetsReady: done,
					AssetsTotal: total,
				})
				mu.Unlock()
			})
			if assetErr != nil {
				mu.Lock()
				launchErrors = append(launchErrors, fmt.Errorf("%s: %w", group.GPUSpec(), assetErr))
				mu.Unlock()
				return
			}

			minReliability := 0.95
			if cfg, err := config.Load(); err == nil && cfg != nil {
				minReliability = cfg.CampaignReliability()
			}
			// Pass the full clients slice into the replacement-offer search
			// so a retryable failure on one provider's offer can fall back
			// to another provider's offer pool. The price gate
			// (searchAuthorizedReplacementOffer) bounds candidates by the
			// user's authorized spend history for this GPU bucket, applied
			// uniformly across same-provider and cross-provider
			// replacements. The matching client swap happens inside
			// createInstanceWithReplacement.
			gateCtx := PriceGateContext{
				GPUClass: group.GPUClass,
				GPUMemGB: group.GPUMemGB,
				JobIDs:   priceGateJobIDs(group),
				DB:       database,
			}
			coverageExclusions := map[string]struct{}{}
			if opts.DistinctMachines {
				var coverageErr error
				coverageExclusions, coverageErr = distinctMachineExclusions(database, campaignID, opts.AvoidMachines)
				if coverageErr != nil {
					mu.Lock()
					launchErrors = append(launchErrors, fmt.Errorf("%s: coverage exclusion: %w", group.GPUSpec(), coverageErr))
					mu.Unlock()
					return
				}
				currentMachineKey := offerMachineClaimKey(ofr)
				for key := range selectedMachineKeys {
					if key != currentMachineKey {
						coverageExclusions[key] = struct{}{}
					}
				}
			}
			affinityMachines := VastAIMachineKeys(opts.AffinityMachines)
			replacementOffer := replacementOfferFunc(func(excludeOfferKeys map[string]struct{}) (*cloud.Offer, error) {
				return searchAuthorizedReplacementOffer(gateCtx, excludeOfferKeys, func(exclude map[string]struct{}) GroupOffer {
					return SearchBestOfferForGroupWithProfileAndMachineExclusions(
						clients,
						group,
						survivalModel,
						jobDurationHrs,
						bidding.ConstantSetup(setupOverheadHrs),
						exclude,
						coverageExclusions,
						affinityMachines,
						opts.ScoringProfile(),
						minReliability,
						opts.MinSurvival,
					)
				})
			})

			maxReplans := retrypolicy.MaxGroupReplans()
			if opts.DistinctMachines || (ofr.Provider == cloud.ProviderRunpod && group.MinDriverVersion > 0) {
				maxReplans += retrypolicy.MaxCreateAttempts() - 1
			}
			var blockers []runpodHeldBlocker
			cID, currentOffer, _, err := runGroupLaunchWithReplan(
				ofr,
				client,
				maxReplans,
				func(c cloud.Client, o cloud.Offer) (int64, error) {
					launchOpts := opts
					launchOpts.holdRunpodDriverBlockers = true
					id, launchErr := LaunchInstance(
						c, clients, database, &campaignID, group, o, launchOpts, r2Cfg, createOpts,
						groupAssets, replacementOffer, progress, instanceRegistered,
					)
					var conflictErr *distinctMachinePostCreateConflictError
					if errors.As(launchErr, &conflictErr) && conflictErr.KeepAlive {
						blockers = append(blockers, runpodHeldBlocker{
							LaunchID:   conflictErr.LaunchID,
							Provider:   conflictErr.Provider,
							ProviderID: conflictErr.ProviderID,
						})
					}
					var driverErr *postCreateDriverCompatibilityError
					if errors.As(launchErr, &driverErr) && driverErr.KeepAlive {
						blockers = append(blockers, runpodHeldBlocker{
							LaunchID:   driverErr.LaunchID,
							Provider:   driverErr.Provider,
							ProviderID: driverErr.ProviderID,
						})
					}
					return id, launchErr
				},
				func() (*cloud.Offer, error) {
					return replacementOffer(map[string]struct{}{})
				},
				func(p cloud.Provider) cloud.Client {
					return clientForProvider(clients, p)
				},
				progress,
			)
			blockerDetail := "replacement accepted; releasing held RunPod pod"
			if err != nil {
				blockerDetail = "launch failed; releasing held RunPod pod"
			}
			destroyRunpodHeldBlockers(database, func(p cloud.Provider) cloud.Client {
				return clientForProvider(clients, p)
			}, blockers, blockerDetail)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// Clean up any jobs assigned to the failed launch so they
				// become eligible for re-launch.
				if cID != 0 {
					_, _ = db.ResetLaunchJobs(database, cID, db.AttemptOutcomeOrphaned)
				}
				launchErrors = append(launchErrors, fmt.Errorf("%s: %w", group.GPUSpec(), err))
				if onEvent != nil {
					onEvent(LaunchEvent{
						Kind:   LaunchEventGroupFailed,
						Group:  group,
						Reason: err.Error(),
					})
				}
			} else {
				instanceIDs = append(instanceIDs, cID)
				recordCampaignPlacementTelemetry(database, appCfg, campaignID, cID, group, currentOffer, estimate, opts.ScoringProfile(), opts.PlacementAlternatives)
				if onEvent != nil {
					onEvent(LaunchEvent{
						Kind:       LaunchEventGroupDone,
						Group:      group,
						InstanceID: cID,
					})
				}
				if donorCfg != nil {
					_ = db.SetLaunchDonorID(database, cID, donorInstanceID)
					// Retrieve the provider ID for this instance
					inst, getErr := db.GetLaunch(database, cID)
					if getErr == nil && inst != nil {
						workerProviderIDs = append(workerProviderIDs, workerInfo{
							ProviderID: inst.EffectiveProviderID(),
							DBID:       cID,
						})
					}
				}
			}
		}(g, offer, groupEstimate, jobDurationHrs, setupOverheadHrs)
	}

	wg.Wait()
	close(stopFirstRegistrationWatch)
	mu.Lock()
	timedOut := firstRegistrationTimedOut
	mu.Unlock()
	if timedOut {
		cancelNonTerminalCampaignInstances(database, campaignID,
			"campaign failed: first worker registration timeout")
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
		return nil, fmt.Errorf(
			"first worker registration timed out after %s (scope=%s sample=%d)",
			firstRegSurvival.TerminateAfter.Truncate(time.Second),
			firstRegSurvival.ScopeDescription(),
			firstRegSurvival.SampleSize,
		)
	}

	// Donor fan-out: wait for readiness, copy caches, then destroy donor
	if donorCfg != nil && donorProviderID != "" && len(workerProviderIDs) > 0 {
		if onEvent != nil {
			onEvent(LaunchEvent{
				Kind:  LaunchEventCampaignStatus,
				Group: InstanceGroup{GPUClass: "donor"},
				Phase: "waiting for donor downloads",
			})
		}

		donorReady := false
		readyKey := r2keys.DonorReady(donorInstanceID)
		downloadStart := time.Now()

		// Poll R2 for donor readiness
		for time.Since(downloadStart) < DefaultDonorReadyTimeout {
			exists, checkErr := stager.Client.ObjectExists(context.Background(), readyKey)
			if checkErr != nil {
				slog.Warn("donor R2 readiness check error", "component", "donor", "error", checkErr)
			} else if exists {
				donorReady = true
				break
			}
			time.Sleep(10 * time.Second)
		}

		if donorReady {
			downloadSecs := int(time.Since(downloadStart).Seconds())
			_ = db.SetLaunchSeedDownloadSecs(database, donorInstanceID, downloadSecs)
			slog.Info("donor ready, starting fan-out", "component", "donor", "download_secs", downloadSecs, "worker_count", len(workerProviderIDs))

			if onEvent != nil {
				onEvent(LaunchEvent{
					Kind:  LaunchEventCampaignStatus,
					Group: InstanceGroup{GPUClass: "donor"},
					Phase: "copying caches to workers",
				})
			}

			var progressFunc func(int64, string)
			if onEvent != nil {
				progressFunc = func(dbID int64, phase string) {
					onEvent(LaunchEvent{
						Kind:  LaunchEventCampaignStatus,
						Group: InstanceGroup{GPUClass: "donor"},
						Phase: fmt.Sprintf("worker %d: %s", dbID, phase),
					})
				}
			}

			if seedErr := SeedWorkers(donorClient, database, donorProviderID, workerProviderIDs, DefaultDonorCachePaths, progressFunc); seedErr != nil {
				slog.Warn("donor fan-out had errors", "component", "donor", "error", seedErr)
			}
		} else {
			slog.Warn("donor timed out waiting for readiness, workers will download independently", "component", "donor")
		}

		// Destroy donor instance
		if onEvent != nil {
			onEvent(LaunchEvent{
				Kind:  LaunchEventCampaignStatus,
				Group: InstanceGroup{GPUClass: "donor"},
				Phase: "destroying donor instance",
			})
		}
		if destroyErr := donorClient.DestroyInstance(donorProviderID); destroyErr != nil {
			slog.Warn("failed to destroy donor instance", "component", "donor", "error", destroyErr)
		}
		_ = db.UpdateLaunchStatus(database, donorInstanceID, db.LaunchStatusCompleted, db.TerminationReasonCompleted)
	}

	// Update campaign status
	if len(instanceIDs) == 0 && len(launchErrors) > 0 {
		cancelNonTerminalCampaignInstances(database, campaignID,
			"campaign failed: no instances launched")
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
	} else {
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusRunning)
	}

	return &LaunchResult{
		CampaignID:  campaignID,
		InstanceIDs: instanceIDs,
		Errors:      launchErrors,
	}, nil
}

// cancelNonTerminalCampaignInstances marks every non-terminal launch in
// a campaign canceled, so CampaignFailed's "all instances terminal"
// invariant holds before the campaign itself is stamped failed.
func cancelNonTerminalCampaignInstances(database *sql.DB, campaignID int64, detail string) {
	instances, err := db.GetCampaignInstances(database, campaignID)
	if err != nil {
		slog.Warn("cancel non-terminal instances: list campaign launches",
			"component", "lifecycle", "campaign_id", campaignID, "error", err)
		return
	}
	for _, inst := range instances {
		if inst == nil || IsInstanceTerminal(inst.Status) {
			continue
		}
		if err := db.UpdateLaunchStatus(database, inst.ID,
			db.LaunchStatusCancelled, db.TerminationReasonCancelled, detail); err != nil {
			slog.Warn("cancel non-terminal instance",
				"component", "lifecycle",
				"campaign_id", campaignID,
				"launch_id", inst.ID,
				"prior_status", inst.Status,
				"error", err)
			continue
		}
		slog.Info("canceled non-terminal launch before campaign failed",
			"component", "lifecycle",
			"campaign_id", campaignID,
			"launch_id", inst.ID,
			"prior_status", inst.Status)
	}
}

// clientForProviderFromList partially-applies clientForProvider so a
// cross-provider lookup can be passed through createInstanceWithReplacement
// without leaking the full clients slice. Returns nil when the slice is
// empty so createInstanceWithReplacement falls back to single-provider
// retries (today's behavior before the cross-provider fix).
func clientForProviderFromList(clients []cloud.Client) func(cloud.Provider) cloud.Client {
	if len(clients) == 0 {
		return nil
	}
	return func(p cloud.Provider) cloud.Client {
		return clientForProvider(clients, p)
	}
}

// clientForProvider finds the client matching a provider from a list.
// If provider is empty, returns the first client as a default. Otherwise
// returns the exact match or nil — never fall back to a different provider,
// since routing a runpod instance to the vastai client (or vice versa)
// corrupts instance lookups and destroys.
func clientForProvider(clients []cloud.Client, provider cloud.Provider) cloud.Client {
	if provider == "" && len(clients) > 0 {
		return clients[0]
	}
	for _, c := range clients {
		if c.Provider() == provider {
			return c
		}
	}
	return nil
}

type runpodHeldBlocker struct {
	LaunchID   int64
	Provider   cloud.Provider
	ProviderID string
}

func destroyRunpodHeldBlockers(database *sql.DB, clientForProvider func(cloud.Provider) cloud.Client, blockers []runpodHeldBlocker, detail string) {
	for _, blocker := range blockers {
		if blocker.LaunchID <= 0 || blocker.ProviderID == "" {
			continue
		}
		client := clientForProvider(blocker.Provider)
		if client == nil {
			slog.Warn("runpod held-blocker cleanup skipped: no provider client",
				"component", "launch",
				"launch_id", blocker.LaunchID,
				"provider", blocker.Provider,
				"provider_id", blocker.ProviderID)
			continue
		}
		if err := client.DestroyInstance(blocker.ProviderID); err != nil {
			slog.Warn("runpod held-blocker cleanup failed",
				"component", "launch",
				"launch_id", blocker.LaunchID,
				"provider", blocker.Provider,
				"provider_id", blocker.ProviderID,
				"error", err)
			oplog.Log(oplog.OpLaunchDestroyFailed,
				oplog.WithError(err),
				oplog.WithDetailf("provider=%s provider_instance_id=%s launch_id=%d context=runpod_held_blocker_cleanup",
					blocker.Provider, blocker.ProviderID, blocker.LaunchID),
			)
			continue
		}
		_ = db.UpdateLaunchStatus(database, blocker.LaunchID, db.LaunchStatusCancelled, db.TerminationReasonCancelled, detail)
	}
}

func supportsDonorStrategy(client cloud.Client) bool {
	return client != nil && client.Provider() == cloud.ProviderVastai
}

func firstRegistrationScopeFromOffers(offers []cloud.Offer) db.FirstRegistrationScope {
	if len(offers) == 0 {
		return db.FirstRegistrationScope{}
	}
	provider := string(offers[0].Provider)
	if provider == "" {
		return db.FirstRegistrationScope{}
	}
	allProvider := true
	dataCenter := offers[0].DataCenter
	allDataCenter := dataCenter != ""
	for _, offer := range offers[1:] {
		if string(offer.Provider) != provider {
			allProvider = false
		}
		if offer.DataCenter != dataCenter {
			allDataCenter = false
		}
	}
	if !allProvider {
		return db.FirstRegistrationScope{}
	}
	scope := db.FirstRegistrationScope{Provider: provider}
	if allDataCenter {
		scope.DataCenter = dataCenter
	}
	return scope
}

func configureBootstrapCreateOpts(client cloud.Client, createOpts *cloud.CreateOpts, bootstrapKey string) error {
	switch client.Provider() {
	case cloud.ProviderRunpod:
		createOpts.OnStartCmd = ""
		if createOpts.TemplateID != "" {
			if createOpts.EnvVars == nil {
				createOpts.EnvVars = map[string]string{}
			}
			createOpts.EnvVars[cloud.R2BootstrapKeyEnvVar] = bootstrapKey
		}
	default:
		createOpts.OnStartCmd = cloud.R2BootstrapOnStartCmd(bootstrapKey)
	}
	return nil
}

func applyGroupCreateRequirements(createOpts *cloud.CreateOpts, group InstanceGroup) {
	if group.MinCUDAVersion != "" {
		createOpts.MinCUDAVersion = maxCUDAVersionString(createOpts.MinCUDAVersion, group.MinCUDAVersion)
	}
	if runpodCloudType := strings.TrimSpace(group.RunpodCloudType); runpodCloudType != "" {
		createOpts.RunpodCloudType = runpodCloudType
	}
	if createOpts.RegistryAuth != nil || createOpts.Image == "" {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Debug("load config for registry auth", "component", "launch", "error", err)
		return
	}
	auth, err := cfg.RegistryAuthForImage(createOpts.Image, group.ImagePullSecret)
	if err != nil {
		slog.Warn("resolve registry auth", "component", "launch", "image", createOpts.Image, "error", err)
		return
	}
	createOpts.RegistryAuth = auth
}

// R2Assets holds pre-staged R2 resources shared across instances in a campaign.
type R2Assets struct {
	Client       *r2.Client
	AgentVersion string            // agent version string (jj commit hash)
	AgentR2Key   string            // R2 key for the agent binary
	SourceR2Keys map[string]string // localDir -> R2 key for source tarballs
}

// runGroupLaunchWithReplan invokes launch up to maxReplans+1 times. The
// first invocation uses the planner's initial offer/client. After each
// retryable failure (and while budget remains), it consults fetchFreshOffer
// for a brand-new initial offer and clientForProvider to align the active
// client, then re-runs the launch with a fresh excludedOffers set inside
// createInstanceWithReplacement. Returns the final cID/offer/client/err.
//
// The replan exists because providers' offer pools turn over within the
// move window — a freshly searched offer can succeed where the previous
// chain's accumulated exclude set could not. The price-gated search used
// for in-chain replacement and outer replan is the same closure, so the
// per-chain offers stay within the user's authorized spend.
func runGroupLaunchWithReplan(
	initialOffer cloud.Offer,
	initialClient cloud.Client,
	maxReplans int,
	launch func(client cloud.Client, offer cloud.Offer) (int64, error),
	fetchFreshOffer func() (*cloud.Offer, error),
	clientForProvider func(cloud.Provider) cloud.Client,
	progress cloud.ProgressFunc,
) (int64, cloud.Offer, cloud.Client, error) {
	currentOffer := initialOffer
	currentClient := initialClient
	var cID int64
	var err error
	for replanIdx := 0; replanIdx <= maxReplans; replanIdx++ {
		if replanIdx > 0 && progress != nil {
			progress(fmt.Sprintf("replanning with fresh offer (chain %d/%d)", replanIdx+1, maxReplans+1))
		}
		cID, err = launch(currentClient, currentOffer)
		if err == nil {
			return cID, currentOffer, currentClient, nil
		}
		if !isRetryableCreateError(err) || replanIdx == maxReplans {
			return cID, currentOffer, currentClient, err
		}
		nextOffer, searchErr := fetchFreshOffer()
		if searchErr != nil || nextOffer == nil {
			return cID, currentOffer, currentClient, err
		}
		nextClient := clientForProvider(nextOffer.Provider)
		if nextClient == nil {
			return cID, currentOffer, currentClient, err
		}
		currentOffer = *nextOffer
		currentClient = nextClient
	}
	return cID, currentOffer, currentClient, err
}

// LaunchInstance creates a cloud instance record, pre-stages assets to R2, and
// bootstraps the instance. Most providers self-bootstrap via onstart, while
// RunPod uses an SSH-triggered bootstrap step after pod creation.
// Returns the cloud instance DB ID.
// LaunchInstance creates one cloud instance for the given group/offer and
// returns the local DB launch ID. The clients slice is consulted only when
// the replacement-offer retry chain pivots to a different provider; pass
// nil when the caller doesn't intend to allow cross-provider fallback
// (e.g. the relaunch path and TUI single-shot launches, which already
// pass nil for replacementOffer).
func LaunchInstance(
	client cloud.Client,
	clients []cloud.Client,
	database *sql.DB,
	campaignID *int64,
	group InstanceGroup,
	offer cloud.Offer,
	opts LaunchOpts,
	r2Cfg cloud.R2Config,
	createOpts cloud.CreateOpts,
	r2Assets R2Assets,
	replacementOffer replacementOfferFunc,
	progress cloud.ProgressFunc,
	onInstanceRegistered func(instanceID int64),
) (int64, error) {
	if progress == nil {
		progress = func(string) {}
	}
	if r2Assets.Client == nil {
		return 0, ErrR2ClientRequired
	}

	// Reject before spending: a job declaring a checkpoint:/corpus: input has
	// no provisioning path onto a rental and would fail inside the job.
	if err := validateGroupCloudProvisionable(group); err != nil {
		return 0, err
	}

	ctx := context.Background()

	// The container's disk allocation is what the provider grants on create
	// (createOpts.DiskGB), bumped up to the group's estimated need if larger.
	// This is what we record on the launch row — not offer.DiskSpaceGB, which
	// is the host machine's total disk and unrelated to what this container
	// actually has. Disk-full diagnostics, relaunch sizing, and offer
	// re-filtering all need the container value.
	requestedDiskGB := createOpts.DiskGB
	if group.DiskGB > 0 && group.DiskGB > requestedDiskGB {
		requestedDiskGB = group.DiskGB
	}
	estimatedDiskGB, diskAnomalies := EstimateGroupDisk(group, database, r2Assets.Client)
	if estimatedDiskGB > requestedDiskGB {
		requestedDiskGB = estimatedDiskGB
		slog.Info("raised launch disk allocation from estimator",
			"component", "launch",
			"gpu_spec", group.GPUSpec(),
			"base_disk_gb", createOpts.DiskGB,
			"group_disk_gb", group.DiskGB,
			"estimated_disk_gb", estimatedDiskGB)
	}
	if len(diskAnomalies) > 0 {
		recordDiskTelemetryAnomalies(database, []InstanceGroup{group}, diskAnomalies)
	}

	// Create cloud instance record with offer metadata
	runpodCloudType := ""
	if client.Provider() == cloud.ProviderRunpod {
		runpodCloudType = strings.TrimSpace(createOpts.RunpodCloudType)
		if runpodCloudType == "" {
			runpodCloudType = cloud.DefaultRunpodCloudType
		}
	}
	instance := &db.Launch{
		CampaignID:        campaignID,
		Status:            db.LaunchStatusPlanned,
		Provider:          string(client.Provider()),
		GPUSpec:           group.GPUSpec(),
		GPUClass:          group.GPUClass,
		GPUMemGB:          int(math.Round(offer.GPUMemGB)),
		MaxSpendCents:     opts.MaxSpendCents,
		MaxTimeSeconds:    opts.MaxTimeSeconds,
		ResolvedGPUName:   offer.GPUName,
		CostPerHourCents:  int(offer.CostPerHour * 100),
		NumGPUs:           offer.NumGPUs,
		DLPerf:            offer.DLPerf,
		Reliability:       offer.Reliability,
		InetDownMbps:      offer.DownloadBandwidth,
		InetUpMbps:        offer.UploadBandwidth,
		CUDAVersion:       offer.CUDAVersion,
		CPUCores:          offer.CPUCores,
		CPUName:           offer.CPUName,
		RAMGB:             offer.RAMGB,
		DiskGB:            requestedDiskGB,
		ProvisionedInputs: group.AllInputs(),
		MachineID:         offer.MachineID,
		InstanceType:      cloud.InstanceTypeOnDemand,
		RunpodCloudType:   runpodCloudType,
	}
	if group.HasPreemptibleJob() {
		instance.InstanceType = cloud.InstanceTypeInterruptible
		bidCents := int(offer.CostPerHour * 100)
		instance.MaxBidPriceCents = &bidCents
	}
	instanceID, err := db.CreateLaunch(database, instance)
	if err != nil {
		return 0, fmt.Errorf("create cloud instance: failed to register local launch record before provider create: %w", err)
	}
	if instance.InstanceType == cloud.InstanceTypeInterruptible {
		go func() {
			if ref := snapshotOnDemandRefCents(client, group, offer); ref != nil {
				if err := db.UpdateLaunchOnDemandRefCents(database, instanceID, *ref); err != nil {
					slog.Debug("record on-demand counterfactual failed", "launch_id", instanceID, "error", err)
				}
			}
		}()
	}
	emitProgress := func(phase string) {
		progress(phase)
		if client.Provider() == cloud.ProviderRunpod {
			recordRunpodObservedBootstrapPhase(database, campaignID, instanceID, phase)
		}
		oplog.Log(oplog.OpLaunchLaunchPhase, oplog.WithDetailf(
			"launch_id=%d provider=%s offer_id=%s phase=%s",
			instanceID,
			client.Provider(),
			offer.ProviderID,
			phase,
		))
	}

	// Transition to launching before assigning jobs so that the job_status
	// view immediately considers assigned jobs as placed.
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusLaunching); err != nil {
		return instanceID, fmt.Errorf("update instance status to launching: %w", err)
	}
	// Stamp the bootstrap deadline so the reconciler reads a single
	// per-launch field. Prefer the learned provider-specific bootstrap
	// threshold when enough history exists.
	bootstrapTimeout := BootstrapTerminateTimeout
	provider := string(client.Provider())
	if survival, err := db.ComputeBootstrapSurvival(database, provider); err != nil {
		slog.Debug("compute bootstrap survival deadline", "component", "launch", "provider", client.Provider(), "error", err)
	} else if survival != nil && survival.TerminateAfter > 0 {
		bootstrapTimeout = survival.TerminateAfter
	}
	if err := db.SetLaunchBootstrapDeadline(database, instanceID, time.Now().Add(bootstrapTimeout)); err != nil {
		slog.Warn("set bootstrap deadline", "component", "launch", "instance_id", instanceID, "error", err)
	}

	if opts.HedgeProbe {
		if opts.HedgeCohortID > 0 {
			if err := db.SetLaunchHedgeCohort(database, instanceID, opts.HedgeCohortID); err != nil {
				slog.Warn("set hedge cohort id", "component", "launch", "instance", instanceID, "cohort", opts.HedgeCohortID, "error", err)
			}
		}
		group.Jobs = nil
	}

	// Associate jobs with cloud instance and record campaign position.
	// Jobs that were claimed by another launch between ListUnplacedJobs and
	// now are skipped rather than causing a hard failure.
	var claimedJobs []*db.Job
	cancelByLaunch := map[int64][]int64{}
	type moveClaim struct {
		intentID  int64
		attemptID int64
	}
	moveClaims := make([]moveClaim, 0, len(group.Jobs))
	rollbackMoveClaims := func(reason string) {
		for _, claim := range moveClaims {
			_ = db.AbandonMoveLoser(database, claim.intentID, claim.attemptID, db.AttemptAbandonedMoveDestinationInfraFailed)
			_ = db.ResolveMoveIntent(database, claim.intentID, db.MoveIntentStateCanceled, reason)
		}
	}
	for i, job := range group.Jobs {
		var prior PriorAttempt
		var updatedJob *db.Job
		if opts.MoveTargetClaim {
			prior = capturePriorAttempt(database, job.ID)
			intent, err := db.GetOpenMoveIntent(database, job.ID)
			if err != nil {
				oplog.Log(oplog.OpCloudSetJobInstance,
					oplog.WithDetailf("job_id: %d, instance_id: %d", job.ID, instanceID),
					oplog.WithError(err),
				)
				rollbackMoveClaims("new instance launch failed before destination acceptance")
				_, _ = db.ResetLaunchJobs(database, instanceID, db.AttemptOutcomeOrphaned)
				return instanceID, err
			}
			if intent == nil {
				err := fmt.Errorf("job %s has no open move intent", ids.FormatJobID(job.ID))
				oplog.Log(oplog.OpCloudSetJobInstance,
					oplog.WithDetailf("job_id: %d, instance_id: %d", job.ID, instanceID),
					oplog.WithError(err),
				)
				rollbackMoveClaims("new instance launch failed before destination acceptance")
				_, _ = db.ResetLaunchJobs(database, instanceID, db.AttemptOutcomeOrphaned)
				return instanceID, err
			}
			attemptID, err := db.CreateMoveTargetAttempt(database, intent.ID, job.ID, "", &instanceID, db.StatusQueued)
			if err != nil {
				oplog.Log(oplog.OpCloudSetJobInstance,
					oplog.WithDetailf("job_id: %d, instance_id: %d", job.ID, instanceID),
					oplog.WithError(err),
				)
				rollbackMoveClaims("new instance launch failed before destination acceptance")
				_, _ = db.ResetLaunchJobs(database, instanceID, db.AttemptOutcomeOrphaned)
				return instanceID, err
			}
			moveClaims = append(moveClaims, moveClaim{intentID: intent.ID, attemptID: attemptID})
			copyJob := *job
			copyJob.Host = ""
			copyJob.LaunchID = &instanceID
			copyJob.LatestRunID = &attemptID
			copyJob.Status = db.StatusQueued
			updatedJob = &copyJob
		} else {
			var err error
			updatedJob, err = claimJobForLaunchWithOpts(database, job.ID, instanceID, false)
			if err != nil {
				if errors.Is(err, db.ErrJobAlreadyClaimed) {
					slog.Info("job claimed by another launch, skipping",
						"component", "launch", "job_id", job.ID, "launch_id", instanceID)
					continue
				}
				oplog.Log(oplog.OpCloudSetJobInstance,
					oplog.WithDetailf("job_id: %d, instance_id: %d", job.ID, instanceID),
					oplog.WithError(err),
				)
				_, _ = db.ResetLaunchJobs(database, instanceID, db.AttemptOutcomeOrphaned)
				return instanceID, err
			}
		}
		group.Jobs[i] = updatedJob
		if err := db.SetJobCampaignIndex(database, job.ID, i); err != nil {
			_, _ = db.ResetLaunchJobs(database, instanceID, db.AttemptOutcomeOrphaned)
			return instanceID, fmt.Errorf("set campaign_job_index for job %s: %w", ids.FormatJobID(job.ID), err)
		}
		claimedJobs = append(claimedJobs, group.Jobs[i])
		if prior.LaunchID > 0 && prior.LaunchID != instanceID && prior.AttemptID > 0 {
			cancelByLaunch[prior.LaunchID] = append(cancelByLaunch[prior.LaunchID], prior.AttemptID)
		}
	}
	_ = cancelByLaunch

	// If all jobs were claimed by other launches, clean up the empty launch.
	// Hedge probes are exempt: they intentionally launch with no jobs.
	if len(claimedJobs) == 0 && !opts.HedgeProbe {
		_ = db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusCancelled,
			db.TerminationReasonCancelled, "all jobs claimed by other launches")
		return instanceID, fmt.Errorf("launch %d: all %d jobs claimed by other launches", instanceID, len(group.Jobs))
	}
	group.Jobs = claimedJobs
	resetLaunchJobsForFailure := func(outcome string, reason string) {
		if opts.MoveTargetClaim {
			rollbackMoveClaims(reason)
		}
		_, _ = db.ResetLaunchJobs(database, instanceID, outcome)
	}

	if onInstanceRegistered != nil {
		onInstanceRegistered(instanceID)
	}

	// Build local-to-remote directory mapping and agent job list.
	localToRemote := make(map[string]string)
	for _, d := range group.SourceDirs() {
		localToRemote[d] = path.Join(cloud.ProjectRootDir, path.Base(d))
	}

	var agentJobs []cloud.AgentJob
	for _, job := range group.Jobs {
		agentJob, err := newCloudAgentJob(job, remoteDirForAgentJob(job, localToRemote))
		if err != nil {
			resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance launch failed before destination acceptance")
			return instanceID, fmt.Errorf("build agent job payload for job %s: %w", ids.FormatJobID(job.ID), err)
		}
		cloudNeeds, cloudAfter, err := resolveCloudNeedsForJob(ctx, database, r2Assets.Client, job, instanceID)
		if err != nil {
			resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance launch failed before destination acceptance")
			return instanceID, err
		}
		var restagedOutputs bool
		cloudNeeds, restagedOutputs, err = appendResumeCloudNeeds(ctx, database, r2Assets.Client, job, cloudNeeds)
		if err != nil {
			resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance launch failed before destination acceptance")
			return instanceID, err
		}
		agentJob.CloudNeeds = cloudNeeds
		agentJob.CloudAfter = cloudAfter
		agentJob.RestagedOutputs = restagedOutputs
		agentJobs = append(agentJobs, agentJob)
	}

	// Sort jobs by ID so the agent executes them in submission order
	sort.Slice(agentJobs, func(i, j int) bool {
		return agentJobs[i].ID < agentJobs[j].ID
	})

	createOpts.DiskGB = requestedDiskGB
	if group.HasPreemptibleJob() {
		createOpts.InstanceType = cloud.InstanceTypeInterruptible
	}

	// Override image if the group resolved a job-specific image.
	if group.Image != "" {
		createOpts.Image = group.Image
	}
	applyGroupCreateRequirements(&createOpts, group)
	if client.Provider() == cloud.ProviderVastai && len(group.VastCapAdd) > 0 {
		createOpts.CapAdd = append([]string(nil), group.VastCapAdd...)
	}

	// Persist the final Docker image for analytics (correlate loading duration vs image type)
	if createOpts.Image != "" {
		if err := db.UpdateLaunchDockerImage(database, instanceID, createOpts.Image); err != nil {
			slog.Warn("failed to persist docker image", "launch_id", instanceID, "error", err)
		}
	}

	// Build the bootstrap key using the DB instance ID (known before CreateInstance)
	bootstrapKey := r2keys.BootstrapScript(instanceID)

	// Build R2 env vars for the instance
	envVars := map[string]string{
		"R2_ACCESS_KEY_ID":     r2Cfg.AccessKeyID,
		"R2_SECRET_ACCESS_KEY": r2Cfg.SecretAccessKey,
		"R2_ENDPOINT":          r2Assets.Client.Endpoint(),
		"R2_BUCKET":            r2Cfg.Bucket,
	}
	// OnStart probe: a presigned PUT URL the container hits before any
	// rclone/apt setup. If this object appears in R2, we know the
	// container ran OnStart and had outbound network. If it doesn't, the
	// container either never ran OnStart or has no outbound at all —
	// disambiguates two failure modes that look identical otherwise.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
	if probeURL, err := r2Assets.Client.PresignPutURL(probeCtx, r2keys.InstanceOnStartProbe(instanceID), 6*time.Hour); err == nil {
		envVars[cloud.OnStartProbeURLEnvVar] = probeURL
	}
	if stageURL, err := r2Assets.Client.PresignPutURL(probeCtx, r2keys.InstanceOnStartStage(instanceID), 6*time.Hour); err == nil {
		envVars[cloud.OnStartStageURLEnvVar] = stageURL
	}
	cancelProbe()
	// Pass Vast.ai API key for self-destruct (read from local config)
	if apiKey := vastai.ReadAPIKey(); apiKey != "" {
		envVars["VASTAI_API_KEY"] = apiKey
	}
	// Forward HF token for gated model downloads
	if token := os.Getenv("HF_TOKEN"); token != "" {
		envVars["HF_TOKEN"] = token
		envVars["HUGGING_FACE_HUB_TOKEN"] = token
	}
	if client.Provider() == cloud.ProviderRunpod {
		if token := os.Getenv("RUNPOD_API_KEY"); token != "" {
			envVars["RUNPOD_API_KEY"] = token
		}
	}

	// Merge env vars into createOpts
	if createOpts.EnvVars == nil {
		createOpts.EnvVars = envVars
	} else {
		for k, v := range envVars {
			createOpts.EnvVars[k] = v
		}
	}

	// Configure provider-specific bootstrap wiring.
	if err := configureBootstrapCreateOpts(client, &createOpts, bootstrapKey); err != nil {
		_ = db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, "bootstrap config failed: "+err.Error())
		resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance bootstrap config failed before destination acceptance")
		oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
			"launch_id=%d reason=infra_failure detail=bootstrap config failed: %s", instanceID, err))
		return instanceID, fmt.Errorf("configure bootstrap: %w", err)
	}

	// Set instance label for provider dashboard visibility and orphan sweep.
	// All weft instances must carry a weft/ prefix so the orphan sweep can identify them.
	if campaignID != nil {
		createOpts.Label = fmt.Sprintf("weft/c%d", *campaignID)
	} else {
		createOpts.Label = fmt.Sprintf("weft/i%d", instanceID)
	}

	campaignLogID := "nil"
	if campaignID != nil {
		campaignLogID = fmt.Sprintf("%d", *campaignID)
	}
	jobIDs := make([]string, 0, len(group.Jobs))
	for _, job := range group.Jobs {
		jobIDs = append(jobIDs, fmt.Sprintf("%d", job.ID))
	}
	oplog.Log(oplog.OpLaunchLaunchRequested, oplog.WithDetailf(
		"launch_id=%d provider=%s campaign_id=%s offer_id=%s jobs=[%s] requested_disk_gb=%d base_disk_gb=%d group_disk_gb=%d offer_disk_gb=%.0f inputs=%d gpu=%s label=%s",
		instanceID,
		client.Provider(),
		campaignLogID,
		offer.ProviderID,
		strings.Join(jobIDs, ","),
		createOpts.DiskGB,
		cloud.DefaultCreateOpts("").DiskGB,
		group.DiskGB,
		offer.DiskSpaceGB,
		len(group.AllInputs()),
		group.GPUSpec(),
		createOpts.Label,
	))

	// Create cloud instance. createInstanceWithReplacement may swap client
	// mid-retry if the replacement offer is from a different provider, and
	// returns the final client so the readback / downstream calls below
	// route to the right provider.
	inst, finalOffer, finalClient, err := createInstanceWithReplacement(
		client,
		clientForProviderFromList(clients),
		group,
		offer,
		createOpts,
		emitProgress,
		func(replacement cloud.Offer) error {
			return db.UpdateLaunchOfferMetadata(database, instanceID, replacement)
		},
		replacementOffer,
	)
	if finalClient != nil {
		client = finalClient
	}
	if err != nil {
		// A price-authorization failure is categorically different from an
		// infra failure. The system correctly refused to spend more than the
		// user authorized for this GPU bucket; no instance was actually
		// created at the provider, and the autopilot's runaway-failure
		// circuit breaker must not count this against the failure tally.
		// Mark the placeholder launch as canceled (not failed), write a
		// structured placement_blocked block for every job in the group so
		// the TUI surfaces an actionable message, and return the typed
		// error so the autopilot caller can route it correctly.
		var authErr *PriceAuthorizationRequiredError
		if errors.As(err, &authErr) {
			_ = db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusCancelled, db.TerminationReasonCancelled, authErr.Error())
			// Use AttemptOutcomeCancelled (not Orphaned) so the runaway
			// breaker and orphan-streak alarm don't count price-
			// authorization holds — they're user-actionable, not
			// infrastructural, and the autopilot tally must not pause for
			// them. See specs/campaign-lifecycle.allium §
			// PriceAuthorizationDoesNotTripCircuitBreaker.
			resetLaunchJobsForFailure(db.AttemptOutcomeCancelled, "new instance price authorization failed before destination acceptance")
			// Build the structured blockreason JSON inline rather than
			// importing internal/blockreason (it depends on this package,
			// so the import would cycle). The shape matches
			// blockreason.Structured.Marshal() output.
			structuredJSON, _ := json.Marshal(struct {
				Summary string `json:"summary"`
				Launch  string `json:"launch,omitempty"`
			}{
				Summary: authErr.Error(),
				Launch:  authErr.Error(),
			})
			for _, job := range group.Jobs {
				if job == nil {
					continue
				}
				_ = db.SetJobPlacementBlocked(database, job.ID, string(structuredJSON))
				_ = db.SetJobPlacementReasons(database, job.ID, []string{authErr.Error()})
			}
			oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
				"launch_id=%d provider=%s requires_price_authorization gpu_class=%s gpu_mem_gb=%d",
				instanceID, client.Provider(), authErr.GPUClass, authErr.GPUMemGB,
			))
			return instanceID, err
		}
		// Distinguish provider CLI/API timeouts from genuine create failures so
		// the clustered-failures banner can ignore them — see
		// db.IsTransientInstanceTermination and internal/ui/terminal/list_grouped_status.go.
		reason := db.TerminationReasonInfraFailure
		if errors.Is(err, cloud.ErrProviderCommandTimeout) {
			reason = db.TerminationReasonProviderTimeout
		}
		detail := "instance creation failed: " + err.Error()
		_ = db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, reason, detail)
		resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance creation failed before destination acceptance")
		oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
			"launch_id=%d provider=%s offer_id=%s error=%s",
			instanceID, client.Provider(), finalOffer.ProviderID, err,
		))
		return instanceID, err
	}

	providerInstID := inst.ProviderID
	// failLaunchInfra performs the shared post-create failure ritual: destroy
	// the leaked provider instance, mark the launch failed with an
	// infra_failure detail, orphan the claimed jobs, and log the failure.
	failLaunchInfra := func(detail string, err error) {
		destroyLeakedInstance(client, providerInstID, instanceID)
		_ = db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, detail+": "+err.Error())
		resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance "+detail+" before destination acceptance")
		oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
			"launch_id=%d reason=infra_failure detail=%s: %s", instanceID, detail, err))
	}
	oplog.Log(oplog.OpLaunchLaunchCreated, oplog.WithDetailf(
		"launch_id=%d provider=%s provider_instance_id=%s requested_disk_gb=%d offer_id=%s status=%s",
		instanceID,
		client.Provider(),
		providerInstID,
		createOpts.DiskGB,
		finalOffer.ProviderID,
		inst.Status,
	))

	// Record provider instance ID before any late distinct-machine rejection so
	// a held blocker pod remains visible and controllable while replacement
	// sampling continues.
	if err := db.SetLaunchProviderID(database, instanceID, providerInstID); err != nil {
		failLaunchInfra("provider ID recording failed", err)
		return instanceID, fmt.Errorf("record provider instance ID: %w", err)
	}
	if err := db.UpdateLaunchInstanceMetadata(database, instanceID, inst); err != nil {
		slog.Debug("record provider instance metadata failed", "launch_id", instanceID, "error", err)
	}
	observedMachineID := strings.TrimSpace(inst.MachineID)
	readback, readbackErr := client.ShowInstance(providerInstID)
	if readbackErr != nil {
		oplog.Log(oplog.OpLaunchLaunchReadback,
			oplog.WithError(readbackErr),
			oplog.WithDetailf(
				"launch_id=%d provider=%s provider_instance_id=%s requested_disk_gb=%d",
				instanceID,
				client.Provider(),
				providerInstID,
				createOpts.DiskGB,
			),
		)
	} else {
		if err := db.UpdateLaunchInstanceMetadata(database, instanceID, readback); err != nil {
			slog.Debug("record provider instance metadata failed", "launch_id", instanceID, "error", err)
		}
		if strings.TrimSpace(readback.MachineID) != "" {
			observedMachineID = strings.TrimSpace(readback.MachineID)
		}
		oplog.Log(oplog.OpLaunchLaunchReadback, oplog.WithDetailf(
			"launch_id=%d provider=%s provider_instance_id=%s requested_disk_gb=%d provider_disk_gb=%.0f status=%s ssh_host=%s ssh_port=%d",
			instanceID,
			client.Provider(),
			providerInstID,
			createOpts.DiskGB,
			readback.DiskGB,
			readback.Status,
			readback.SSHHost,
			readback.SSHPort,
		))
		if createOpts.DiskGB > 0 && readback.DiskGB > 0 && math.Abs(readback.DiskGB-float64(createOpts.DiskGB)) >= 1 {
			oplog.Log(oplog.OpLaunchLaunchMismatch, oplog.WithDetailf(
				"launch_id=%d provider=%s provider_instance_id=%s requested_disk_gb=%d provider_disk_gb=%.0f offer_disk_gb=%.0f",
				instanceID,
				client.Provider(),
				providerInstID,
				createOpts.DiskGB,
				readback.DiskGB,
				finalOffer.DiskSpaceGB,
			))
		}
	}

	if opts.DistinctMachines && client.Provider() == cloud.ProviderRunpod {
		if opts.distinctAcceptMu != nil {
			opts.distinctAcceptMu.Lock()
		}
		conflict, err := runpodDistinctMachineConflict(database, campaignID, instanceID, observedMachineID, opts.AvoidMachines, opts.distinctAcceptedByMach)
		if err != nil {
			if opts.distinctAcceptMu != nil {
				opts.distinctAcceptMu.Unlock()
			}
			failLaunchInfra("distinct machine check failed", err)
			return instanceID, fmt.Errorf("distinct machine check: %w", err)
		}
		if conflict != "" {
			err := &distinctMachinePostCreateConflictError{
				LaunchID:   instanceID,
				Provider:   client.Provider(),
				ProviderID: providerInstID,
				MachineKey: db.ProviderMachineKey(string(cloud.ProviderRunpod), observedMachineID),
				Reason:     conflict,
				KeepAlive:  observedMachineID != "",
			}
			if err.KeepAlive {
				_ = db.SetLaunchCordoned(database, instanceID, true, conflict)
				resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance sampled non-distinct RunPod machine before destination acceptance")
				emitProgress(conflict + "; holding pod and searching again")
				oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
					"launch_id=%d provider=%s provider_instance_id=%s reason=distinct_machine_conflict detail=%s",
					instanceID, client.Provider(), providerInstID, conflict))
				if opts.distinctAcceptMu != nil {
					opts.distinctAcceptMu.Unlock()
				}
				return instanceID, err
			}
			destroyLeakedInstance(client, providerInstID, instanceID)
			_ = db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, conflict)
			resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance missing RunPod machine identity before destination acceptance")
			oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
				"launch_id=%d provider=%s provider_instance_id=%s reason=missing_machine_id",
				instanceID, client.Provider(), providerInstID))
			if opts.distinctAcceptMu != nil {
				opts.distinctAcceptMu.Unlock()
			}
			return instanceID, err
		}
		if opts.distinctAcceptMu != nil {
			opts.distinctAcceptMu.Unlock()
		}
	}

	if err := runpodPostCreateDriverCompatibility(
		ctx,
		r2Assets.Client,
		client,
		instanceID,
		providerInstID,
		observedMachineID,
		group.MinDriverVersion,
		opts.holdRunpodDriverBlockers,
		emitProgress,
	); err != nil {
		var driverErr *postCreateDriverCompatibilityError
		if errors.As(err, &driverErr) && driverErr.KeepAlive {
			reason := err.Error()
			_ = db.SetLaunchCordoned(database, instanceID, true, reason)
			_, _ = db.SetLaunchLiveInstancePhase(database, instanceID, phaseDriverTooOld)
			resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance sampled incompatible RunPod driver before destination acceptance")
			emitProgress(reason + "; holding pod and searching again")
			oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
				"launch_id=%d provider=%s provider_instance_id=%s reason=driver_too_old detail=%s",
				instanceID, client.Provider(), providerInstID, reason))
			return instanceID, err
		}
		_, _ = db.SetLaunchLiveInstancePhase(database, instanceID, phaseDriverTooOld)
		failLaunchInfra("driver compatibility rejected", err)
		return instanceID, err
	}

	// Record data center if available
	if finalOffer.DataCenter != "" {
		_ = db.SetLaunchDataCenter(database, instanceID, finalOffer.DataCenter)
	}

	// Generate and upload bootstrap script (must happen after CreateInstance
	// so we have providerInstID for self-destruct, but before instance finishes
	// booting and runs onstart-cmd — boot typically takes several minutes).
	emitProgress("uploading bootstrap")

	// Build source mappings for the bootstrap script
	var sources []SourceMapping
	for localDir, remoteDir := range localToRemote {
		if r2Key, ok := r2Assets.SourceR2Keys[localDir]; ok {
			sources = append(sources, SourceMapping{
				R2Key:     r2Key,
				RemoteDir: remoteDir,
				LocalDir:  localDir,
			})
		}
	}

	// Store grace period in DB if configured
	if opts.GracePeriodSeconds > 0 {
		_ = db.SetLaunchGracePeriod(database, instanceID, opts.GracePeriodSeconds)
	}

	// Generate and upload campaign manifest (needs providerInstID for self-destruct)
	selfDestructCmd := client.SelfDestructCmd(providerInstID)
	manifest := cloud.CampaignManifest{
		Jobs:                agentJobs,
		SelfDestructCmd:     selfDestructCmd,
		SkipWorkdirDeletion: opts.SkipWorkdirDeletion,
		GPUWarmup:           opts.GPUWarmup,
		CostPerHourCents:    int(offer.CostPerHour * 100),
		Provider:            string(client.Provider()),
		InstanceType:        instance.InstanceType,
		RequestedDiskGB:     requestedDiskGB,
		RequiredDriverMajor: group.MinDriverVersion,
		Drain:               drainSettingsFromConfig(),
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		failLaunchInfra("manifest generation failed", err)
		return instanceID, fmt.Errorf("generate campaign manifest: %w", err)
	}

	manifestKey := r2keys.CampaignManifest(instanceID)
	if err := r2Assets.Client.PutObject(ctx, manifestKey, bytes.NewReader(manifestJSON), "application/json"); err != nil {
		failLaunchInfra("manifest upload failed", err)
		return instanceID, fmt.Errorf("upload campaign manifest: %w", err)
	}

	// Store agent version for diagnostics
	if r2Assets.AgentVersion != "" {
		versionKey := r2keys.InstanceAgentVersion(instanceID)
		_ = r2Assets.Client.PutObject(ctx, versionKey, strings.NewReader(r2Assets.AgentVersion), "text/plain")
	}

	bootstrapScript := GenerateBootstrapScript(BootstrapManifest{
		AgentR2Key:         r2Assets.AgentR2Key,
		Sources:            sources,
		Image:              createOpts.Image,
		HFModels:           collectHFModels([]InstanceGroup{group}),
		HFDatasets:         collectHFDatasets([]InstanceGroup{group}),
		DBInstanceID:       instanceID,
		MaxTimeSeconds:     opts.MaxTimeSeconds,
		GracePeriodSeconds: opts.GracePeriodSeconds,
		AgentForeground:    client.Provider() == cloud.ProviderRunpod && createOpts.TemplateID != "",
	})

	if err := r2Assets.Client.PutObject(ctx, bootstrapKey, strings.NewReader(bootstrapScript), "text/x-shellscript"); err != nil {
		failLaunchInfra("bootstrap upload failed", err)
		return instanceID, fmt.Errorf("upload bootstrap script: %w", err)
	}

	if client.Provider() == cloud.ProviderRunpod && createOpts.TemplateID == "" {
		type runpodBootstrapper interface {
			BootstrapFromR2(context.Context, string, string, string) error
		}
		type runpodProgressBootstrapper interface {
			BootstrapFromR2WithProgress(context.Context, string, string, string, cloud.ProgressFunc) error
		}
		bootstrapper, ok := client.(runpodBootstrapper)
		if !ok {
			destroyLeakedInstance(client, providerInstID, instanceID)
			err := fmt.Errorf("runpod client does not support SSH bootstrap")
			_ = db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, err.Error())
			resetLaunchJobsForFailure(db.AttemptOutcomeOrphaned, "new instance runpod bootstrap unsupported before destination acceptance")
			oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
				"launch_id=%d reason=infra_failure detail=%s", instanceID, err))
			return instanceID, err
		}
		emitProgress("runpod ssh bootstrap")
		sshCtx, cancel := context.WithTimeout(ctx, runpodSSHBootstrapTimeout)
		defer cancel()
		var err error
		if progressBootstrapper, ok := client.(runpodProgressBootstrapper); ok {
			err = progressBootstrapper.BootstrapFromR2WithProgress(sshCtx, providerInstID, r2Cfg.Bucket, bootstrapKey, emitProgress)
		} else {
			err = bootstrapper.BootstrapFromR2(sshCtx, providerInstID, r2Cfg.Bucket, bootstrapKey)
		}
		if err != nil {
			failLaunchInfra("runpod ssh bootstrap failed", err)
			return instanceID, fmt.Errorf("runpod ssh bootstrap: %w", err)
		}
	}

	// Update status to running — instance is now self-starting
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusRunning); err != nil {
		if errors.Is(err, db.ErrLaunchTerminal) {
			// The instance was terminated (e.g. user cancel) while the
			// launch was finishing. Terminal status is sticky; the terminate
			// path already destroyed the provider instance and reset the
			// jobs, so report the launch as not running without re-failing
			// the row.
			oplog.Log(oplog.OpLaunchTerminated, oplog.WithDetailf(
				"launch_id=%d detail=terminated during launch; final running write skipped", instanceID))
			return instanceID, fmt.Errorf("instance %d terminated during launch: %w", instanceID, err)
		}
		return instanceID, fmt.Errorf("update instance status: %w", err)
	}

	return instanceID, nil
}

// destroyLeakedInstance attempts to destroy a provider instance that was created
// but couldn't be fully registered. Logs a warning if destruction fails, since a
// silent failure here causes the instance to leak (incurring ongoing charges).
func destroyLeakedInstance(client cloud.Client, providerInstID string, launchID int64) {
	if err := client.DestroyInstance(providerInstID); err != nil {
		slog.Warn("failed to destroy leaked instance, manual cleanup required", "component", "launch", "provider", client.Provider(), "provider_id", providerInstID, "instance", launchID, "error", err)
		oplog.Log(oplog.OpLaunchDestroyFailed,
			oplog.WithError(err),
			oplog.WithDetailf("provider=%s provider_instance_id=%s launch_id=%d context=cleanup_after_launch_failure",
				client.Provider(), providerInstID, launchID),
		)
	}
}

// drainSettingsFromConfig returns the upload-drain tunables that travel in
// the campaign manifest, sourced from the user's ~/.config/weft/config.toml
// [cloud.drain] section. Empty fields are left zero so the agent falls
// back to the r2upload package defaults.
func drainSettingsFromConfig() cloud.DrainSettings {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return cloud.DrainSettings{}
	}
	d := cfg.Cloud.Drain
	return cloud.DrainSettings{
		StallTimeoutSeconds:        d.StallTimeoutSeconds,
		InitialStallTimeoutSeconds: d.InitialStallTimeoutSeconds,
		HeartbeatTimeoutSeconds:    d.HeartbeatTimeoutSeconds,
		FloorThroughputBytesPerSec: d.FloorThroughputBytesPerSec,
		MaxDrainSeconds:            d.MaxDrainSeconds,
		BaselineSeconds:            d.BaselineSeconds,
		MarkerTimeoutSeconds:       d.MarkerTimeoutSeconds,
		PaceCheckAfterSeconds:      d.PaceCheckAfterSeconds,
		MinThroughputFraction:      d.MinThroughputFraction,
	}
}
