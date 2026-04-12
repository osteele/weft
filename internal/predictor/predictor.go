// Package predictor shells out to the job-estimator Python CLI to train
// models and predict job duration, peak RSS, and peak GPU memory.
package predictor

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Config controls how the predictor finds and invokes job-estimator.
type Config struct {
	// ProjectPath is the path to the job-estimator Python project checkout.
	ProjectPath string `yaml:"project_path"`
	// ModelDir is where trained models are stored.
	// Default: ~/.cache/weft/models
	ModelDir string `yaml:"model_dir"`
	// RetrainInterval is how many new completed jobs trigger a retrain.
	// Default: 50
	RetrainInterval int `yaml:"retrain_interval"`
	// DBPaths lists job database paths to feed into training.
	// The weft DB (~/.config/weft/jobs.db) is always included.
	DBPaths []string `yaml:"db_paths"`
}

// Prediction holds a point estimate with uncertainty bounds.
type Prediction struct {
	Mean            float64 `json:"mean"`
	Std             float64 `json:"std"`
	Lower           float64 `json:"lower"`
	Upper           float64 `json:"upper"`
	EpistemicFactor float64 `json:"epistemic_factor,omitempty"`
	NCalibration    int     `json:"n_calibration,omitempty"`
}

// RuntimeMetadata describes how a duration prediction was produced.
type RuntimeMetadata struct {
	Source                     string  `json:"source,omitempty"`
	Confidence                 float64 `json:"confidence,omitempty"`
	Feasible                   *bool   `json:"feasible,omitempty"`
	Bottleneck                 string  `json:"bottleneck,omitempty"`
	MemoryHeadroomMiB          float64 `json:"memory_headroom_mib,omitempty"`
	BenefitsFromAdditionalVRAM *bool   `json:"benefits_from_additional_vram,omitempty"`
	AnalyticalDurationS        float64 `json:"analytical_duration_s,omitempty"`
	AnalyticalPeakMemoryMiB    float64 `json:"analytical_peak_memory_mib,omitempty"`
}

// Status describes the current usability and refresh state of predictor models.
type Status struct {
	Configured                 bool
	Ready                      bool
	ModelDir                   string
	ModelAvailable             bool
	TrainedAt                  string
	JobCount                   int
	CurrentJobCount            int
	NewCompletedJobs           int
	RetrainInterval            int
	Stale                      bool
	SchemaIncompatible         bool
	SchemaReason               string
	BackgroundRebuildRunning   bool
	BackgroundRebuildReason    string
	BackgroundRebuildStartedAt time.Time
	CountError                 string
	MetaError                  string
}

type estimatorSnapshot struct {
	TotalRows            int  `json:"total_rows"`
	DurationTrainingRows int  `json:"duration_training_rows"`
	PeakRSSRows          int  `json:"peak_rss_rows"`
	MaxGPUMemRows        int  `json:"max_gpu_mem_rows"`
	MaxRunID             *int `json:"max_run_id"`
	MaxCreatedAt         *int `json:"max_created_at"`
}

type estimatorModelStatus struct {
	Ready                     bool               `json:"ready"`
	ModelPresent              bool               `json:"model_present"`
	SchemaCompatible          bool               `json:"schema_compatible"`
	SchemaReason              string             `json:"schema_reason"`
	SchemaVersion             *int               `json:"schema_version"`
	ExpectedSchemaVersion     int                `json:"expected_schema_version"`
	TrainedAt                 string             `json:"trained_at"`
	JobCount                  *int               `json:"job_count"`
	TrainingSnapshot          *estimatorSnapshot `json:"training_snapshot"`
	CurrentSnapshot           estimatorSnapshot  `json:"current_snapshot"`
	SnapshotChanged           bool               `json:"snapshot_changed"`
	DurationRowsSinceTraining int                `json:"duration_rows_since_training"`
	RetrainInterval           int                `json:"retrain_interval"`
	Stale                     bool               `json:"stale"`
}

// Result holds predictions for all targets.
type Result struct {
	DurationS        *Prediction      `json:"duration_s"`
	DurationMetadata *RuntimeMetadata `json:"duration_metadata,omitempty"`
	PeakRSSKB        *Prediction      `json:"peak_rss_kb"`
	MaxGPUMemMiB     *Prediction      `json:"max_gpu_mem_mib"`
}

// Meta is the sidecar metadata written alongside trained models.
type Meta struct {
	TrainedAt     string         `json:"trained_at"`
	JobCount      int            `json:"job_count"`
	DBPaths       []string       `json:"db_paths"`
	Models        map[string]any `json:"models"`
	SchemaVersion int            `json:"schema_version,omitempty"`
}

const ExpectedModelSchemaVersion = 3

var modelArtifactNames = []string{"duration", "peak_rss_kb", "max_gpu_mem_mib"}

type ModelSchemaStatus struct {
	Changed bool
	Reason  string
}

// UnavailableError reports that predictor models cannot be used right now.
type UnavailableError struct {
	Status Status
}

func (e *UnavailableError) Error() string {
	if e == nil {
		return "predictor unavailable"
	}
	if e.Status.SchemaIncompatible {
		if e.Status.BackgroundRebuildRunning {
			return fmt.Sprintf(
				"predictor models use an incompatible schema (%s); current model use is blocked while a background rebuild is in progress",
				e.Status.SchemaReason,
			)
		}
		return fmt.Sprintf(
			"predictor models use an incompatible schema (%s); current model use is blocked until rebuilt",
			e.Status.SchemaReason,
		)
	}
	return "predictor unavailable"
}

var predictFunc = Predict
var predictBatchFunc = PredictBatch
var runPredictCLI = runPredictCLIImpl
var runPredictBatchCLI = runPredictBatchCLIImpl
var runTrainCLI = runTrainCLIImpl
var runStatusCLI = runStatusCLIImpl
var countTrainingRows = countTrainingRowsImpl
var backgroundRetrainCheckInterval = time.Minute
var predictorStatusCacheTTL = 5 * time.Second
var nowFunc = time.Now
var predictionCache = struct {
	mu      sync.RWMutex
	entries map[predictionCacheKey]*Result
}{
	entries: make(map[predictionCacheKey]*Result),
}
var backgroundRetrains = struct {
	mu     sync.Mutex
	states map[string]*backgroundRetrainState
}{
	states: make(map[string]*backgroundRetrainState),
}

type predictionCacheKey struct {
	modelDir string
	host     string
	project  string
	gpuClass string
	command  string
}

type backgroundRetrainState struct {
	useMu          sync.RWMutex
	statusMu       sync.Mutex
	running        bool
	reason         string
	startedAt      time.Time
	nextStaleCheck time.Time
	cachedStatus   Status
	cachedAt       time.Time
}

// ResolvePredict routes prediction calls through the package test seam.
func ResolvePredict(cfg Config, host, project, gpuClass, command string) (*Result, error) {
	return predictFunc(cfg, host, project, gpuClass, command)
}

// ResolvePredictBatch routes batch prediction calls through the package test seam.
func ResolvePredictBatch(cfg Config, jobs []BatchJob) (map[int64]*Result, error) {
	return predictBatchFunc(cfg, jobs)
}

// Configured returns true if the predictor has a project path set.
func (c *Config) Configured() bool {
	return c.ProjectPath != ""
}

// BuildConfig creates a Config and ensures the weft DB is always included
// in DBPaths. This is the canonical way to construct a predictor Config
// from application config values.
func BuildConfig(projectPath, modelDir string, retrainInterval int, dbPaths []string) Config {
	cfg := Config{
		ProjectPath:     projectPath,
		ModelDir:        modelDir,
		RetrainInterval: retrainInterval,
		DBPaths:         append([]string(nil), dbPaths...),
	}

	home, err := os.UserHomeDir()
	if err == nil {
		weftDB := filepath.Join(home, ".config", "weft", "jobs.db")
		if !slices.Contains(cfg.DBPaths, weftDB) {
			cfg.DBPaths = append(cfg.DBPaths, weftDB)
		}
	}

	return cfg
}

func (c *Config) modelDir() string {
	if c.ModelDir != "" {
		return c.ModelDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "weft", "models")
}

func (c *Config) retrainInterval() int {
	if c.RetrainInterval > 0 {
		return c.RetrainInterval
	}
	return 50
}

func (c *Config) metaPath() string {
	return filepath.Join(c.modelDir(), "meta.json")
}

// ReadMeta reads the model metadata sidecar file.
func ReadMeta(cfg Config) (*Meta, error) {
	data, err := os.ReadFile(cfg.metaPath())
	if err != nil {
		return nil, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// GetStatus reports predictor readiness, model freshness, and any background rebuild.
func GetStatus(cfg Config) Status {
	status := baseStatus(cfg)
	state := backgroundState(cfg)
	state.statusMu.Lock()
	status.BackgroundRebuildRunning = state.running
	status.BackgroundRebuildReason = state.reason
	status.BackgroundRebuildStartedAt = state.startedAt
	state.statusMu.Unlock()
	return status
}

func baseStatus(cfg Config) Status {
	status := Status{
		Configured:      cfg.Configured(),
		ModelDir:        cfg.modelDir(),
		RetrainInterval: cfg.retrainInterval(),
	}
	if !status.Configured {
		return status
	}

	state := backgroundState(cfg)
	state.statusMu.Lock()
	if !state.cachedAt.IsZero() && nowFunc().Sub(state.cachedAt) < predictorStatusCacheTTL {
		cached := state.cachedStatus
		state.statusMu.Unlock()
		return cached
	}
	state.statusMu.Unlock()

	if estimatorStatus, err := loadEstimatorStatus(cfg); err == nil {
		status.ModelAvailable = estimatorStatus.ModelPresent
		status.TrainedAt = estimatorStatus.TrainedAt
		if estimatorStatus.JobCount != nil {
			status.JobCount = *estimatorStatus.JobCount
		}
		status.CurrentJobCount = estimatorStatus.CurrentSnapshot.DurationTrainingRows
		status.NewCompletedJobs = estimatorStatus.DurationRowsSinceTraining
		status.Stale = estimatorStatus.Stale
		status.SchemaIncompatible = !estimatorStatus.SchemaCompatible
		status.SchemaReason = estimatorStatus.SchemaReason
		status.Ready = estimatorStatus.Ready
	} else {
		if meta, metaErr := ReadMeta(cfg); metaErr == nil {
			status.ModelAvailable = true
			status.TrainedAt = meta.TrainedAt
			status.JobCount = meta.JobCount
		} else {
			status.MetaError = metaErr.Error()
		}

		schema := localModelSchemaStatus(cfg)
		status.SchemaIncompatible = schema.Changed
		status.SchemaReason = schema.Reason

		if currentJobCount, countErr := countTrainingRows(cfg); countErr == nil {
			status.CurrentJobCount = currentJobCount
			if status.ModelAvailable {
				status.NewCompletedJobs = max(0, currentJobCount-status.JobCount)
				status.Stale = NeedsRetrain(cfg, currentJobCount)
			}
		} else {
			status.CountError = countErr.Error()
		}
		status.Ready = status.ModelAvailable && !status.SchemaIncompatible
	}

	state.statusMu.Lock()
	state.cachedStatus = status
	state.cachedAt = nowFunc()
	state.statusMu.Unlock()
	return status
}

// EnsureReady checks whether the configured predictor models are usable.
// Stale-but-compatible models trigger a background rebuild while remaining usable.
func EnsureReady(cfg Config) error {
	return preparePredictorForUse(cfg)
}

// ModelSchemaStatus reports whether the trained-model artifacts match the
// current expected on-disk schema.
func CheckModelSchema(cfg Config) ModelSchemaStatus {
	status := baseStatus(cfg)
	if status.SchemaIncompatible {
		return ModelSchemaStatus{
			Changed: true,
			Reason:  status.SchemaReason,
		}
	}
	return ModelSchemaStatus{}
}

func localModelSchemaStatus(cfg Config) ModelSchemaStatus {
	modelDir := cfg.modelDir()
	if modelDir == "" {
		return ModelSchemaStatus{}
	}

	meta, err := ReadMeta(cfg)
	if err == nil && meta.SchemaVersion > 0 && meta.SchemaVersion != ExpectedModelSchemaVersion {
		return ModelSchemaStatus{
			Changed: true,
			Reason:  fmt.Sprintf("model schema version is %d (expected %d)", meta.SchemaVersion, ExpectedModelSchemaVersion),
		}
	}
	if err == nil && meta.SchemaVersion == 0 {
		return ModelSchemaStatus{
			Changed: true,
			Reason:  fmt.Sprintf("model schema version is missing (expected %d)", ExpectedModelSchemaVersion),
		}
	}

	for _, name := range modelArtifactNames {
		path := filepath.Join(modelDir, fmt.Sprintf("%s.joblib", name))
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		if !info.IsDir() {
			return ModelSchemaStatus{
				Changed: true,
				Reason:  fmt.Sprintf("model artifact %s uses the legacy file layout", filepath.Base(path)),
			}
		}
	}

	return ModelSchemaStatus{}
}

func loadEstimatorStatus(cfg Config) (*estimatorModelStatus, error) {
	if cfg.ProjectPath == "" {
		return nil, fmt.Errorf("predictor: project_path not configured")
	}
	if _, err := os.Stat(cfg.ProjectPath); err != nil {
		return nil, fmt.Errorf("predictor: project_path unavailable: %w", err)
	}
	out, err := runStatusCLI(cfg)
	if err != nil {
		return nil, err
	}
	var status estimatorModelStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return nil, fmt.Errorf("predictor: parse status output: %w", err)
	}
	return &status, nil
}

func removeModelArtifacts(cfg Config) error {
	modelDir := cfg.modelDir()
	if modelDir == "" {
		return nil
	}

	paths := []string{cfg.metaPath()}
	for _, name := range modelArtifactNames {
		paths = append(paths, filepath.Join(modelDir, fmt.Sprintf("%s.joblib", name)))
	}
	for _, path := range paths {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func clearPredictionCache() {
	predictionCache.mu.Lock()
	defer predictionCache.mu.Unlock()
	predictionCache.entries = make(map[predictionCacheKey]*Result)
}

func invalidateStatusCache(cfg Config) {
	state := backgroundState(cfg)
	state.statusMu.Lock()
	state.cachedStatus = Status{}
	state.cachedAt = time.Time{}
	state.statusMu.Unlock()
}

func predictionKey(cfg Config, host, project, gpuClass, command string) predictionCacheKey {
	return predictionCacheKey{
		modelDir: cfg.modelDir(),
		host:     host,
		project:  project,
		gpuClass: gpuClass,
		command:  command,
	}
}

func cachedPrediction(key predictionCacheKey) (*Result, bool) {
	predictionCache.mu.RLock()
	defer predictionCache.mu.RUnlock()
	result, ok := predictionCache.entries[key]
	if !ok {
		return nil, false
	}
	return cloneResult(result), true
}

func storePrediction(key predictionCacheKey, result *Result) {
	if result == nil {
		return
	}
	predictionCache.mu.Lock()
	defer predictionCache.mu.Unlock()
	predictionCache.entries[key] = cloneResult(result)
}

func clonePrediction(pred *Prediction) *Prediction {
	if pred == nil {
		return nil
	}
	cloned := *pred
	return &cloned
}

func cloneResult(result *Result) *Result {
	if result == nil {
		return nil
	}
	var metadata *RuntimeMetadata
	if result.DurationMetadata != nil {
		cloned := *result.DurationMetadata
		metadata = &cloned
	}
	return &Result{
		DurationS:        clonePrediction(result.DurationS),
		DurationMetadata: metadata,
		PeakRSSKB:        clonePrediction(result.PeakRSSKB),
		MaxGPUMemMiB:     clonePrediction(result.MaxGPUMemMiB),
	}
}

func backgroundState(cfg Config) *backgroundRetrainState {
	modelDir := cfg.modelDir()
	backgroundRetrains.mu.Lock()
	defer backgroundRetrains.mu.Unlock()
	state, ok := backgroundRetrains.states[modelDir]
	if ok {
		return state
	}
	state = &backgroundRetrainState{}
	backgroundRetrains.states[modelDir] = state
	return state
}

// NeedsRetrain returns true if models are stale or missing.
// It compares the job_count in meta.json against currentJobCount.
func NeedsRetrain(cfg Config, currentJobCount int) bool {
	meta, err := ReadMeta(cfg)
	if err != nil {
		return true // Missing or corrupt meta → retrain
	}
	return currentJobCount-meta.JobCount >= cfg.retrainInterval()
}

// Train shells out to job-estimator train with the configured DB paths.
func Train(cfg Config) error {
	if cfg.ProjectPath == "" {
		return fmt.Errorf("predictor: project_path not configured")
	}
	modelDir := cfg.modelDir()
	if modelDir == "" {
		return fmt.Errorf("predictor: model_dir not configured")
	}
	parentDir := filepath.Dir(modelDir)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("predictor: create model dir parent: %w", err)
	}
	tempDir, err := os.MkdirTemp(parentDir, filepath.Base(modelDir)+".retrain-*")
	if err != nil {
		return fmt.Errorf("predictor: create temp model dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	if err := runTrainCLI(cfg, tempDir); err != nil {
		return err
	}

	state := backgroundState(cfg)
	state.useMu.Lock()
	defer state.useMu.Unlock()
	if err := swapModelDir(tempDir, modelDir); err != nil {
		return err
	}
	clearPredictionCache()
	invalidateStatusCache(cfg)
	return nil
}

// Predict shells out to job-estimator predict and parses the JSON result.
func Predict(cfg Config, host, project, gpuClass, command string) (*Result, error) {
	if cfg.ProjectPath == "" {
		return nil, fmt.Errorf("predictor: project_path not configured")
	}
	if err := preparePredictorForUse(cfg); err != nil {
		return nil, err
	}
	key := predictionKey(cfg, host, project, gpuClass, command)
	if result, ok := cachedPrediction(key); ok {
		return result, nil
	}

	state := backgroundState(cfg)
	state.useMu.RLock()
	out, err := runPredictCLI(cfg, host, project, gpuClass, command)
	state.useMu.RUnlock()
	if err != nil {
		return nil, err
	}

	var result Result
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("predictor: parse output: %w", err)
	}
	storePrediction(key, &result)
	return &result, nil
}

// BatchJob describes a single job for batch prediction.
type BatchJob struct {
	ID       int64  `json:"id"`
	Command  string `json:"command"`
	Host     string `json:"host"`
	Project  string `json:"project"`
	GPUClass string `json:"gpu_class"`
}

// batchResultEntry is the JSON shape returned by predict-batch per job.
type batchResultEntry struct {
	ID int64 `json:"id"`
	Result
}

// PredictBatch shells out to job-estimator predict-batch, sending all jobs
// in a single subprocess invocation. Returns a map from job ID to Result.
func PredictBatch(cfg Config, jobs []BatchJob) (map[int64]*Result, error) {
	if cfg.ProjectPath == "" {
		return nil, fmt.Errorf("predictor: project_path not configured")
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	if err := preparePredictorForUse(cfg); err != nil {
		return nil, err
	}

	results := make(map[int64]*Result, len(jobs))
	missingJobs := make([]BatchJob, 0, len(jobs))
	keysByBatchID := make(map[int64]predictionCacheKey)
	idsByKey := make(map[predictionCacheKey][]int64)
	for _, job := range jobs {
		key := predictionKey(cfg, job.Host, job.Project, job.GPUClass, job.Command)
		if cached, ok := cachedPrediction(key); ok {
			results[job.ID] = cached
			continue
		}
		if _, seen := idsByKey[key]; !seen {
			missingJobs = append(missingJobs, job)
			keysByBatchID[job.ID] = key
		}
		idsByKey[key] = append(idsByKey[key], job.ID)
	}
	if len(missingJobs) == 0 {
		return results, nil
	}

	state := backgroundState(cfg)
	state.useMu.RLock()
	out, err := runPredictBatchCLI(cfg, missingJobs)
	state.useMu.RUnlock()
	if err != nil {
		return nil, err
	}

	var entries []batchResultEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("predictor: parse batch output: %w", err)
	}

	for _, e := range entries {
		key, ok := keysByBatchID[e.ID]
		if !ok {
			continue
		}
		r := e.Result
		storePrediction(key, &r)
		for _, id := range idsByKey[key] {
			results[id] = cloneResult(&r)
		}
	}
	return results, nil
}

func preparePredictorForUse(cfg Config) error {
	if status := CheckModelSchema(cfg); status.Changed {
		if err := rebuildSchemaMismatchSynchronously(cfg, status.Reason); err != nil {
			blocked := GetStatus(cfg)
			blocked.SchemaIncompatible = true
			blocked.SchemaReason = status.Reason
			return &UnavailableError{Status: blocked}
		}
		return nil
	}
	maybeScheduleBackgroundRetrain(cfg)
	return nil
}

func rebuildSchemaMismatchSynchronously(cfg Config, reason string) error {
	state := backgroundState(cfg)
	rebuildReason := "schema changed: " + reason

	for {
		state.statusMu.Lock()
		if !state.running {
			state.cachedAt = time.Time{}
			state.cachedStatus = Status{}
			state.running = true
			state.reason = rebuildReason
			state.startedAt = nowFunc()
			state.statusMu.Unlock()
			break
		}
		state.statusMu.Unlock()

		time.Sleep(50 * time.Millisecond)
		if status := CheckModelSchema(cfg); !status.Changed {
			return nil
		}
	}

	slog.Warn(
		"predictor models use an incompatible schema; rebuilding synchronously before prediction",
		"component", "predictor",
		"model_dir", cfg.modelDir(),
		"reason", reason,
	)
	err := Train(cfg)

	state.statusMu.Lock()
	state.running = false
	state.reason = ""
	state.startedAt = time.Time{}
	state.nextStaleCheck = nowFunc().Add(backgroundRetrainCheckInterval)
	state.cachedAt = time.Time{}
	state.cachedStatus = Status{}
	state.statusMu.Unlock()

	if err != nil {
		return fmt.Errorf("predictor: rebuild incompatible models: %w", err)
	}
	return nil
}

func maybeScheduleBackgroundRetrain(cfg Config) {
	state := backgroundState(cfg)
	now := nowFunc()

	state.statusMu.Lock()
	if state.running || (!state.nextStaleCheck.IsZero() && now.Before(state.nextStaleCheck)) {
		state.statusMu.Unlock()
		return
	}
	state.nextStaleCheck = now.Add(backgroundRetrainCheckInterval)
	state.statusMu.Unlock()

	status := GetStatus(cfg)
	if status.SchemaIncompatible {
		return
	}
	if status.CountError != "" {
		slog.Debug(
			"predictor stale-check skipped",
			"component", "predictor",
			"error", status.CountError,
		)
		return
	}
	if !status.Stale {
		return
	}

	reason := "stale models"
	if status.NewCompletedJobs > 0 {
		reason = fmt.Sprintf(
			"%d new duration-training rows since last training",
			status.NewCompletedJobs,
		)
	}
	maybeStartBackgroundRetrain(cfg, reason)
}

func maybeStartBackgroundRetrain(cfg Config, reason string) bool {
	state := backgroundState(cfg)
	state.statusMu.Lock()
	if state.running {
		state.statusMu.Unlock()
		return false
	}
	state.cachedAt = time.Time{}
	state.cachedStatus = Status{}
	state.running = true
	state.reason = reason
	state.startedAt = nowFunc()
	state.statusMu.Unlock()

	go func() {
		slog.Info("starting predictor retrain in background", "component", "predictor", "model_dir", cfg.modelDir(), "reason", reason)
		err := Train(cfg)
		if err != nil {
			slog.Warn("background predictor retrain failed", "component", "predictor", "model_dir", cfg.modelDir(), "reason", reason, "error", err)
		} else {
			slog.Info("background predictor retrain completed", "component", "predictor", "model_dir", cfg.modelDir(), "reason", reason)
		}

		state.statusMu.Lock()
		state.running = false
		state.reason = ""
		state.startedAt = time.Time{}
		state.nextStaleCheck = nowFunc().Add(backgroundRetrainCheckInterval)
		state.cachedAt = time.Time{}
		state.cachedStatus = Status{}
		state.statusMu.Unlock()
	}()
	return true
}

func countTrainingRowsImpl(cfg Config) (int, error) {
	paths := slices.Clone(cfg.DBPaths)
	if len(paths) == 0 {
		return 0, fmt.Errorf("predictor: no training databases configured")
	}

	seen := make(map[string]struct{}, len(paths))
	total := 0
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		count, err := countTrainingRowsInDB(path)
		if err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

func countTrainingRowsInDB(path string) (int, error) {
	if ext := filepath.Ext(path); ext != ".db" && ext != ".sqlite" && ext != ".sqlite3" && ext != "" {
		return 0, fmt.Errorf("predictor: unsupported training data source for stale checks: %s", path)
	}
	connStr := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		return 0, fmt.Errorf("predictor: open training db %s: %w", path, err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return 0, fmt.Errorf("predictor: open training db %s: %w", path, err)
	}

	queries := []string{
		`SELECT COUNT(*) FROM training_examples
		 WHERE duration_s > 5
		   AND LOWER(TRIM(COALESCE(status, ''))) IN ('', 'completed')
		   AND (exit_code IS NULL OR exit_code = 0)
		   AND TRIM(COALESCE(failure_reason, '')) = ''`,
		`SELECT COUNT(*) FROM job_attempts
		 WHERE start_time IS NOT NULL
		   AND end_time IS NOT NULL
		   AND (end_time - start_time) > 5
		   AND LOWER(TRIM(COALESCE(status, ''))) IN ('', 'completed')
		   AND (exit_code IS NULL OR exit_code = 0)
		   AND TRIM(COALESCE(failure_reason, '')) = ''`,
		`SELECT COUNT(*) FROM jobs
		 WHERE start_time IS NOT NULL
		   AND end_time IS NOT NULL
		   AND (end_time - start_time) > 5
		   AND LOWER(TRIM(COALESCE(status, ''))) IN ('', 'completed')
		   AND (exit_code IS NULL OR exit_code = 0)
		   AND TRIM(COALESCE(failure_reason, '')) = ''`,
	}

	var lastErr error
	for _, query := range queries {
		var count int
		if err := db.QueryRow(query).Scan(&count); err == nil {
			return count, nil
		} else {
			lastErr = err
		}
	}
	return 0, fmt.Errorf("predictor: count training rows in %s: %w", path, lastErr)
}

func runTrainCLIImpl(cfg Config, modelDir string) error {
	args := []string{"run", "--project", cfg.ProjectPath, "job-estimator", "train"}
	for _, db := range cfg.DBPaths {
		args = append(args, "--db", db)
	}
	args = append(args, "--model-dir", modelDir)

	cmd := exec.Command("uv", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if summary := summarizeSubprocessStderr(stderr.Bytes(), 280); summary != "" {
			return fmt.Errorf("predictor train: %w: %s", err, summary)
		}
		return err
	}
	if summary := summarizeSubprocessStderr(stderr.Bytes(), 280); summary != "" {
		slog.Debug("predictor train stderr", "stderr", summary)
	}
	return nil
}

func runStatusCLIImpl(cfg Config) ([]byte, error) {
	args := []string{
		"run",
		"--project",
		cfg.ProjectPath,
		"job-estimator",
		"status",
		"--model-dir",
		cfg.modelDir(),
		"--retrain-interval",
		fmt.Sprintf("%d", cfg.retrainInterval()),
	}
	for _, db := range cfg.DBPaths {
		args = append(args, "--db", db)
	}

	cmd := exec.Command("uv", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("predictor status: %w", err)
	}
	if stderr.Len() > 0 {
		slog.Debug("predictor status stderr", "stderr", stderr.String())
	}
	return stdout.Bytes(), nil
}

func summarizeSubprocessStderr(stderr []byte, maxLen int) string {
	clean := bytes.TrimSpace(stderr)
	if len(clean) == 0 {
		return ""
	}
	collapsed := string(bytes.Join(bytes.Fields(clean), []byte(" ")))
	if maxLen <= 0 || len(collapsed) <= maxLen {
		return collapsed
	}
	if maxLen == 1 {
		return "…"
	}
	return collapsed[:maxLen-1] + "…"
}

func swapModelDir(srcDir, dstDir string) error {
	parentDir := filepath.Dir(dstDir)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("predictor: create model dir parent: %w", err)
	}

	backupDir := filepath.Join(parentDir, filepath.Base(dstDir)+".backup")
	_ = os.RemoveAll(backupDir)
	if _, err := os.Stat(dstDir); err == nil {
		if err := os.Rename(dstDir, backupDir); err != nil {
			return fmt.Errorf("predictor: move current model dir aside: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("predictor: stat current model dir: %w", err)
	}

	if err := os.Rename(srcDir, dstDir); err != nil {
		if _, backupErr := os.Stat(backupDir); backupErr == nil {
			_ = os.Rename(backupDir, dstDir)
		}
		return fmt.Errorf("predictor: activate retrained models: %w", err)
	}
	if _, err := os.Stat(backupDir); err == nil {
		if err := os.RemoveAll(backupDir); err != nil {
			return fmt.Errorf("predictor: remove old model backup: %w", err)
		}
	}
	return nil
}

func runPredictCLIImpl(cfg Config, host, project, gpuClass, command string) ([]byte, error) {
	args := []string{
		"run", "--project", cfg.ProjectPath, "job-estimator", "predict",
		"--model-dir", cfg.modelDir(),
		"--command", command,
	}
	if host != "" {
		args = append(args, "--host", host)
	}
	if project != "" {
		args = append(args, "--project", project)
	}
	if gpuClass != "" {
		args = append(args, "--gpu-class", gpuClass)
	}

	cmd := exec.Command("uv", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("predictor predict: %w", err)
	}
	return out, nil
}

func runPredictBatchCLIImpl(cfg Config, jobs []BatchJob) ([]byte, error) {
	input, err := json.Marshal(jobs)
	if err != nil {
		return nil, fmt.Errorf("predictor: marshal batch input: %w", err)
	}

	args := []string{
		"run", "--project", cfg.ProjectPath, "job-estimator", "predict-batch",
		"--model-dir", cfg.modelDir(),
	}

	cmd := exec.Command("uv", args...)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("predictor predict-batch: %w", err)
	}
	return out, nil
}

// EnsureAndPredict retrains if stale, then predicts.
// Errors are non-fatal: returns nil result if prediction fails.
func EnsureAndPredict(cfg Config, currentJobCount int, host, project, gpuClass, command string) *Result {
	if status := CheckModelSchema(cfg); status.Changed {
		if err := rebuildSchemaMismatchSynchronously(cfg, status.Reason); err != nil {
			return nil
		}
	} else if NeedsRetrain(cfg, currentJobCount) {
		_ = maybeStartBackgroundRetrain(cfg, fmt.Sprintf("%d new completed jobs since last training", currentJobCount))
	}
	result, err := predictFunc(cfg, host, project, gpuClass, command)
	if err != nil {
		return nil
	}
	return result
}

// PredictedGPUMemGB converts a max_gpu_mem_mib prediction into a whole-GiB
// reservation using the prediction's upper bound.
func PredictedGPUMemGB(p *Prediction) (int, bool) {
	if p == nil || p.Upper <= 0 {
		return 0, false
	}
	return int(math.Ceil(p.Upper / 1024.0)), true
}

// standardVRAMTiers lists common GPU VRAM sizes in GB, ordered ascending.
// Used to snap predicted memory to the next available tier for ceiling caps.
var standardVRAMTiers = []int{12, 16, 24, 48, 80, 141}

// PredictedGPUMemCeilingGB snaps a GPU memory prediction's upper bound to the
// next standard VRAM tier. This provides a ceiling that prevents over-provisioning
// — e.g., a job predicted to need 20GB gets capped at the 24GB tier, avoiding
// placement on 80GB+ GPUs that provide no benefit.
//
// Returns (0, false) if the prediction is nil or has no upper bound.
func PredictedGPUMemCeilingGB(p *Prediction) (int, bool) {
	if p == nil || p.Upper <= 0 {
		return 0, false
	}
	upperGB := p.Upper / 1024.0
	for _, tier := range standardVRAMTiers {
		if float64(tier) >= upperGB {
			return tier, true
		}
	}
	// Above all known tiers — no ceiling
	return 0, false
}

// ResolveGPUMem returns the effective GPU memory floor and ceiling for a job.
// Explicit floors win for placement, but a predictor-derived ceiling is still
// returned when available so the estimator can ignore performance gains from
// oversized GPUs. Without an explicit floor, the floor uses the maximum of the
// predictor, OOM floor, and fallback. The ceiling snaps the predictor's upper
// bound to the next standard VRAM tier.
// Returns (floor, ceiling, predicted) where ceiling is nil if no prediction available.
func ResolveGPUMem(cfg Config, explicit *int, needsGPU bool, host, project, gpuClass, command string, fallbackGB int, oomFloorGB int) (floor *int, ceiling *int, predicted bool) {
	hasGPURequest := needsGPU || explicit != nil
	if !hasGPURequest {
		return nil, nil, false
	}

	var predictedGB int
	var ceilingGB int
	if cfg.Configured() && command != "" {
		result, err := predictFunc(cfg, host, project, gpuClass, command)
		if err == nil && result != nil {
			if memGB, ok := PredictedGPUMemGB(result.MaxGPUMemMiB); ok {
				predictedGB = memGB
				predicted = true
			}
			if capGB, ok := PredictedGPUMemCeilingGB(result.MaxGPUMemMiB); ok {
				ceilingGB = capGB
			}
		}
	}

	if explicit != nil {
		memGB := *explicit
		var ceilingPtr *int
		if ceilingGB > 0 {
			if ceilingGB < memGB {
				ceilingGB = memGB
			}
			ceilingPtr = &ceilingGB
		}
		return explicit, ceilingPtr, false
	}

	memGB := max(predictedGB, oomFloorGB, fallbackGB)
	if memGB <= 0 {
		return nil, nil, false
	}

	floorPtr := &memGB
	var ceilingPtr *int
	if ceilingGB > 0 {
		// Ensure ceiling is at least as large as the floor
		if ceilingGB < memGB {
			ceilingGB = memGB
		}
		ceilingPtr = &ceilingGB
	}

	return floorPtr, ceilingPtr, predicted && predictedGB == memGB
}

// ResolveGPUMemGB returns the effective GPU memory reservation for a job.
// Explicit reservations win. Otherwise, the OOM floor (from prior failures),
// predictor output, and fallbackGB are considered — the maximum wins.
func ResolveGPUMemGB(cfg Config, explicit *int, needsGPU bool, host, project, gpuClass, command string, fallbackGB int, oomFloorGB int) (*int, bool) {
	floor, _, predicted := ResolveGPUMem(cfg, explicit, needsGPU, host, project, gpuClass, command, fallbackGB, oomFloorGB)
	return floor, predicted
}

// FormatDuration formats a duration prediction as a human-readable string.
func FormatDuration(p *Prediction) string {
	if p == nil {
		return "unknown"
	}
	d := time.Duration(p.Mean) * time.Second
	lower := time.Duration(p.Lower) * time.Second
	upper := time.Duration(p.Upper) * time.Second
	return fmt.Sprintf("~%s (95%% CI: %s – %s)", formatDur(d), formatDur(lower), formatDur(upper))
}

// FormatMemory formats a memory prediction (in the given unit) as a human-readable string.
func FormatMemory(p *Prediction, unit string) string {
	if p == nil {
		return "unknown"
	}
	var divisor float64
	switch unit {
	case "GiB":
		divisor = 1024
	case "GB":
		divisor = 1000
	default:
		divisor = 1
	}
	return fmt.Sprintf("~%.1f %s (95%% CI: %.1f – %.1f %s)",
		p.Mean/divisor, unit, p.Lower/divisor, p.Upper/divisor, unit)
}

func formatDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}
