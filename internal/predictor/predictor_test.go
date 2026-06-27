package predictor

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildConfig(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	weftDB := filepath.Join(home, ".config", "weft", "jobs.db")

	t.Run("auto appends weft DB", func(t *testing.T) {
		cfg := BuildConfig("/path/to/project", "/models", 50, []string{"/other/db.sqlite"})
		if !slices.Contains(cfg.DBPaths, weftDB) {
			t.Errorf("weft DB %q not found in DBPaths: %v", weftDB, cfg.DBPaths)
		}
	})

	t.Run("no duplicates when already present", func(t *testing.T) {
		cfg := BuildConfig("/path/to/project", "/models", 50, []string{weftDB})
		count := 0
		for _, p := range cfg.DBPaths {
			if p == weftDB {
				count++
			}
		}
		if count != 1 {
			t.Errorf("weft DB appears %d times, want 1", count)
		}
	})

	t.Run("empty dbPaths gets weft DB", func(t *testing.T) {
		cfg := BuildConfig("/path", "/models", 50, nil)
		if len(cfg.DBPaths) != 1 || cfg.DBPaths[0] != weftDB {
			t.Errorf("expected [%q], got %v", weftDB, cfg.DBPaths)
		}
	})

	t.Run("does not mutate input slice", func(t *testing.T) {
		input := []string{"/a.db"}
		_ = BuildConfig("/path", "/models", 50, input)
		if len(input) != 1 {
			t.Errorf("input slice was mutated: %v", input)
		}
	})
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		pred *Prediction
		want string
	}{
		{"nil prediction", nil, "unknown"},
		{"seconds", &Prediction{Mean: 45, Lower: 30, Upper: 60}, "~45s (95% CI: 30s – 1m)"},
		{"minutes", &Prediction{Mean: 300, Lower: 240, Upper: 360}, "~5m (95% CI: 4m – 6m)"},
		{"hours", &Prediction{Mean: 7200, Lower: 3600, Upper: 10800}, "~2h (95% CI: 1h – 3h)"},
		{"hours and minutes", &Prediction{Mean: 5400, Lower: 3600, Upper: 7200}, "~1h 30m (95% CI: 1h – 2h)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDuration(tt.pred)
			if got != tt.want {
				t.Errorf("FormatDuration() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatMemory(t *testing.T) {
	tests := []struct {
		name string
		pred *Prediction
		unit string
		want string
	}{
		{"nil prediction", nil, "GiB", "unknown"},
		{"GiB", &Prediction{Mean: 2048, Lower: 1024, Upper: 3072}, "GiB", "~2.0 GiB (95% CI: 1.0 – 3.0 GiB)"},
		{"MiB", &Prediction{Mean: 512, Lower: 256, Upper: 768}, "MiB", "~512.0 MiB (95% CI: 256.0 – 768.0 MiB)"},
		{"GB", &Prediction{Mean: 2000, Lower: 1000, Upper: 3000}, "GB", "~2.0 GB (95% CI: 1.0 – 3.0 GB)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatMemory(tt.pred, tt.unit)
			if got != tt.want {
				t.Errorf("FormatMemory() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatDur(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"below minute", 30 * time.Second, "30s"},
		{"exactly one minute", time.Minute, "1m"},
		{"59 seconds", 59 * time.Second, "59s"},
		{"exactly one hour", time.Hour, "1h"},
		{"hour and minutes", 90 * time.Minute, "1h 30m"},
		{"zero", 0, "0s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDur(tt.d)
			if got != tt.want {
				t.Errorf("formatDur(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestPredictionJSONParsing(t *testing.T) {
	t.Run("with epistemic fields", func(t *testing.T) {
		data := `{"mean":120.5,"std":30.2,"lower":60.1,"upper":180.9,"epistemic_factor":2.5,"n_calibration":45}`
		var p Prediction
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.Mean != 120.5 {
			t.Errorf("Mean = %v, want 120.5", p.Mean)
		}
		if p.EpistemicFactor != 2.5 {
			t.Errorf("EpistemicFactor = %v, want 2.5", p.EpistemicFactor)
		}
		if p.NCalibration != 45 {
			t.Errorf("NCalibration = %v, want 45", p.NCalibration)
		}
	})

	t.Run("backward compat without epistemic fields", func(t *testing.T) {
		data := `{"mean":120.5,"std":30.2,"lower":60.1,"upper":180.9}`
		var p Prediction
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.Mean != 120.5 {
			t.Errorf("Mean = %v, want 120.5", p.Mean)
		}
		if p.EpistemicFactor != 0 {
			t.Errorf("EpistemicFactor = %v, want 0 (zero value)", p.EpistemicFactor)
		}
		if p.NCalibration != 0 {
			t.Errorf("NCalibration = %v, want 0 (zero value)", p.NCalibration)
		}
	})

	t.Run("full result with epistemic", func(t *testing.T) {
		data := `{
			"duration_s": {"mean":120.5,"std":30.2,"lower":60.1,"upper":180.9,"epistemic_factor":1.8,"n_calibration":50},
			"duration_metadata": {"source":"learned+analytical","confidence":0.625,"feasible":true,"bottleneck":"compute","memory_headroom_mib":6144.0,"benefits_from_additional_vram":false,"analytical_duration_s":90.0,"analytical_peak_memory_mib":2048.0},
			"peak_rss_kb": {"mean":1024.0,"std":100.0,"lower":824.0,"upper":1224.0},
			"max_gpu_mem_mib": null
		}`
		var r Result
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if r.DurationS == nil {
			t.Fatal("DurationS is nil")
		}
		if r.DurationS.EpistemicFactor != 1.8 {
			t.Errorf("DurationS.EpistemicFactor = %v, want 1.8", r.DurationS.EpistemicFactor)
		}
		if r.DurationS.NCalibration != 50 {
			t.Errorf("DurationS.NCalibration = %v, want 50", r.DurationS.NCalibration)
		}
		if r.DurationMetadata == nil {
			t.Fatal("DurationMetadata is nil")
		}
		if r.DurationMetadata.Source != "learned+analytical" {
			t.Errorf("DurationMetadata.Source = %q, want learned+analytical", r.DurationMetadata.Source)
		}
		if r.DurationMetadata.Confidence != 0.625 {
			t.Errorf("DurationMetadata.Confidence = %v, want 0.625", r.DurationMetadata.Confidence)
		}
		if r.DurationMetadata.Feasible == nil || !*r.DurationMetadata.Feasible {
			t.Errorf("DurationMetadata.Feasible = %v, want true", r.DurationMetadata.Feasible)
		}
		if r.DurationMetadata.Bottleneck != "compute" {
			t.Errorf("DurationMetadata.Bottleneck = %q, want compute", r.DurationMetadata.Bottleneck)
		}
		if r.DurationMetadata.MemoryHeadroomMiB != 6144.0 {
			t.Errorf("DurationMetadata.MemoryHeadroomMiB = %v, want 6144", r.DurationMetadata.MemoryHeadroomMiB)
		}
		if r.DurationMetadata.BenefitsFromAdditionalVRAM == nil || *r.DurationMetadata.BenefitsFromAdditionalVRAM {
			t.Errorf("DurationMetadata.BenefitsFromAdditionalVRAM = %v, want false", r.DurationMetadata.BenefitsFromAdditionalVRAM)
		}
		if r.PeakRSSKB == nil {
			t.Fatal("PeakRSSKB is nil")
		}
		if r.PeakRSSKB.EpistemicFactor != 0 {
			t.Errorf("PeakRSSKB.EpistemicFactor = %v, want 0", r.PeakRSSKB.EpistemicFactor)
		}
		if r.MaxGPUMemMiB != nil {
			t.Errorf("MaxGPUMemMiB = %v, want nil", r.MaxGPUMemMiB)
		}
	})
}

func TestNeedsRetrain(t *testing.T) {
	t.Run("missing meta file triggers retrain", func(t *testing.T) {
		cfg := Config{
			ProjectPath:     "/path",
			ModelDir:        t.TempDir(),
			RetrainInterval: 50,
		}
		if !NeedsRetrain(cfg, 100) {
			t.Error("expected NeedsRetrain=true for missing meta")
		}
	})

	t.Run("stale meta triggers retrain", func(t *testing.T) {
		dir := t.TempDir()
		meta := Meta{TrainedAt: "2024-01-01", JobCount: 50}
		data, _ := json.Marshal(meta)
		os.WriteFile(filepath.Join(dir, "meta.json"), data, 0644)

		cfg := Config{
			ProjectPath:     "/path",
			ModelDir:        dir,
			RetrainInterval: 50,
		}
		// 100 current - 50 trained = 50, which is >= interval of 50
		if !NeedsRetrain(cfg, 100) {
			t.Error("expected NeedsRetrain=true for stale meta")
		}
	})

	t.Run("fresh meta does not trigger retrain", func(t *testing.T) {
		dir := t.TempDir()
		meta := Meta{TrainedAt: "2024-01-01", JobCount: 80}
		data, _ := json.Marshal(meta)
		os.WriteFile(filepath.Join(dir, "meta.json"), data, 0644)

		cfg := Config{
			ProjectPath:     "/path",
			ModelDir:        dir,
			RetrainInterval: 50,
		}
		// 100 current - 80 trained = 20, which is < 50
		if NeedsRetrain(cfg, 100) {
			t.Error("expected NeedsRetrain=false for fresh meta")
		}
	})
}

func TestCheckModelSchema(t *testing.T) {
	writeMeta := func(t *testing.T, dir string, meta Meta) {
		t.Helper()
		data, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("marshal meta: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), data, 0644); err != nil {
			t.Fatalf("write meta: %v", err)
		}
	}

	t.Run("missing schema version is incompatible", func(t *testing.T) {
		dir := t.TempDir()
		writeMeta(t, dir, Meta{
			TrainedAt: "2024-01-01",
			JobCount:  80,
		})

		status := CheckModelSchema(Config{ProjectPath: "/path", ModelDir: dir})
		if !status.Changed {
			t.Fatal("expected missing schema version to be incompatible")
		}
	})

	t.Run("legacy file layout is incompatible", func(t *testing.T) {
		dir := t.TempDir()
		writeMeta(t, dir, Meta{
			TrainedAt:     "2024-01-01",
			JobCount:      80,
			SchemaVersion: ExpectedModelSchemaVersion,
		})
		if err := os.WriteFile(filepath.Join(dir, "duration.joblib"), []byte("legacy"), 0644); err != nil {
			t.Fatalf("write legacy model: %v", err)
		}

		status := CheckModelSchema(Config{ProjectPath: "/path", ModelDir: dir})
		if !status.Changed {
			t.Fatal("expected legacy file layout to be incompatible")
		}
	})

	t.Run("current schema with directory artifacts is compatible", func(t *testing.T) {
		dir := t.TempDir()
		writeMeta(t, dir, Meta{
			TrainedAt:     "2024-01-01",
			JobCount:      80,
			SchemaVersion: ExpectedModelSchemaVersion,
		})
		for _, name := range modelArtifactNames {
			path := filepath.Join(dir, fmt.Sprintf("%s.joblib", name))
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatalf("mkdir %s: %v", path, err)
			}
		}

		status := CheckModelSchema(Config{ProjectPath: "/path", ModelDir: dir})
		if status.Changed {
			t.Fatalf("expected current schema to be compatible, got %q", status.Reason)
		}
	})
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(message)
}

func TestPredict_StartsBackgroundRetrainWhenStale(t *testing.T) {
	clearPredictionCache()
	backgroundRetrains.mu.Lock()
	backgroundRetrains.states = make(map[string]*backgroundRetrainState)
	backgroundRetrains.mu.Unlock()

	originalRunPredictCLI := runPredictCLI
	originalRunTrainCLI := runTrainCLI
	originalCountTrainingRows := countTrainingRows
	originalInterval := backgroundRetrainCheckInterval
	t.Cleanup(func() {
		runPredictCLI = originalRunPredictCLI
		runTrainCLI = originalRunTrainCLI
		countTrainingRows = originalCountTrainingRows
		backgroundRetrainCheckInterval = originalInterval
		clearPredictionCache()
		backgroundRetrains.mu.Lock()
		backgroundRetrains.states = make(map[string]*backgroundRetrainState)
		backgroundRetrains.mu.Unlock()
	})

	modelDir := t.TempDir()
	meta := Meta{
		TrainedAt:     "2024-01-01T00:00:00Z",
		JobCount:      50,
		SchemaVersion: ExpectedModelSchemaVersion,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "meta.json"), data, 0644); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	backgroundRetrainCheckInterval = 0
	countTrainingRows = func(Config) (int, error) { return 120, nil }
	runPredictCLI = func(_ Config, _, _, _, _ string) ([]byte, error) {
		return []byte(`{"duration_s":{"mean":3600,"std":60,"lower":3500,"upper":3700}}`), nil
	}

	started := make(chan string, 1)
	runTrainCLI = func(_ Config, trainModelDir string) error {
		started <- trainModelDir
		meta := Meta{
			TrainedAt:     "2024-01-02T00:00:00Z",
			JobCount:      120,
			SchemaVersion: ExpectedModelSchemaVersion,
		}
		data, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(trainModelDir, "meta.json"), data, 0644); err != nil {
			return err
		}
		return nil
	}

	cfg := Config{ProjectPath: "/tmp/job-estimator", ModelDir: modelDir, RetrainInterval: 50}
	result, err := Predict(cfg, "", "proj", "RTX 4090", "python train.py")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if result == nil || result.DurationS == nil || result.DurationS.Mean != 3600 {
		t.Fatalf("unexpected prediction: %#v", result)
	}

	select {
	case trainModelDir := <-started:
		if trainModelDir == modelDir {
			t.Fatalf("expected background retrain to use a temp model dir, got %q", trainModelDir)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background retrain did not start")
	}

	waitFor(t, 2*time.Second, func() bool {
		retrainedMeta, err := ReadMeta(cfg)
		return err == nil && retrainedMeta.JobCount == 120
	}, "background retrain did not finish swapping models")
}

func TestPredict_RebuildsSynchronouslyWhenSchemaChanges(t *testing.T) {
	clearPredictionCache()
	backgroundRetrains.mu.Lock()
	backgroundRetrains.states = make(map[string]*backgroundRetrainState)
	backgroundRetrains.mu.Unlock()

	originalRunPredictCLI := runPredictCLI
	originalRunTrainCLI := runTrainCLI
	t.Cleanup(func() {
		runPredictCLI = originalRunPredictCLI
		runTrainCLI = originalRunTrainCLI
		clearPredictionCache()
		backgroundRetrains.mu.Lock()
		backgroundRetrains.states = make(map[string]*backgroundRetrainState)
		backgroundRetrains.mu.Unlock()
	})

	modelDir := t.TempDir()
	meta := Meta{TrainedAt: "2024-01-01T00:00:00Z", JobCount: 50, SchemaVersion: ExpectedModelSchemaVersion - 1}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "meta.json"), data, 0644); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	var predictCalls atomic.Int32
	runPredictCLI = func(_ Config, _, _, _, _ string) ([]byte, error) {
		predictCalls.Add(1)
		return []byte(`{"duration_s":{"mean":3600,"std":60,"lower":3500,"upper":3700}}`), nil
	}

	var trainDir string
	runTrainCLI = func(_ Config, trainModelDir string) error {
		trainDir = trainModelDir
		meta := Meta{
			TrainedAt:     "2024-01-02T00:00:00Z",
			JobCount:      50,
			SchemaVersion: ExpectedModelSchemaVersion,
		}
		data, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(trainModelDir, "meta.json"), data, 0644); err != nil {
			return err
		}
		return nil
	}

	cfg := Config{ProjectPath: "/tmp/job-estimator", ModelDir: modelDir}
	result, err := Predict(cfg, "", "proj", "RTX 4090", "python train.py")
	if err != nil {
		t.Fatalf("expected synchronous rebuild + prediction to succeed, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result after synchronous schema rebuild")
	}
	if result.DurationS == nil || result.DurationS.Mean != 3600 {
		t.Fatalf("duration_s mean = %v, want 3600", result.DurationS)
	}
	if trainDir == modelDir {
		t.Fatalf("expected schema rebuild to use a temp dir, got model dir %q", modelDir)
	}
	if predictCalls.Load() != 1 {
		t.Fatalf("runPredictCLI calls = %d, want 1", predictCalls.Load())
	}

	retrainedMeta, err := ReadMeta(cfg)
	if err != nil {
		t.Fatalf("read meta after rebuild: %v", err)
	}
	if retrainedMeta.SchemaVersion != ExpectedModelSchemaVersion {
		t.Fatalf("schema version = %d, want %d", retrainedMeta.SchemaVersion, ExpectedModelSchemaVersion)
	}
}

func TestTrainSerializesConcurrentSubprocesses(t *testing.T) {
	originalRunTrainCLI := runTrainCLI
	t.Cleanup(func() {
		runTrainCLI = originalRunTrainCLI
	})

	modelDir := t.TempDir()
	var mu sync.Mutex
	active := 0
	maxActive := 0
	runTrainCLI = func(_ Config, trainModelDir string) error {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()

		time.Sleep(100 * time.Millisecond)
		meta := Meta{
			TrainedAt:     "2024-01-02T00:00:00Z",
			JobCount:      50,
			SchemaVersion: ExpectedModelSchemaVersion,
		}
		data, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(trainModelDir, "meta.json"), data, 0644); err != nil {
			return err
		}
		return nil
	}

	cfg := Config{ProjectPath: "/tmp/job-estimator", ModelDir: modelDir}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- Train(cfg)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Train: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("max concurrent train subprocesses = %d, want 1", maxActive)
	}
}

func TestSchemaRebuildSkipsWhenLockedModelAlreadyCompatible(t *testing.T) {
	originalRunTrainCLI := runTrainCLI
	t.Cleanup(func() {
		runTrainCLI = originalRunTrainCLI
	})

	modelDir := t.TempDir()
	meta := Meta{
		TrainedAt:     "2024-01-02T00:00:00Z",
		JobCount:      50,
		SchemaVersion: ExpectedModelSchemaVersion,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "meta.json"), data, 0644); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	var trainCalls atomic.Int32
	runTrainCLI = func(_ Config, _ string) error {
		trainCalls.Add(1)
		return nil
	}

	cfg := Config{ProjectPath: "/tmp/job-estimator", ModelDir: modelDir}
	if err := trainSchemaRebuild(cfg); err != nil {
		t.Fatalf("trainSchemaRebuild: %v", err)
	}
	if trainCalls.Load() != 0 {
		t.Fatalf("train calls = %d, want 0", trainCalls.Load())
	}
}

func TestGetStatusReportsBackgroundRebuild(t *testing.T) {
	backgroundRetrains.mu.Lock()
	backgroundRetrains.states = make(map[string]*backgroundRetrainState)
	backgroundRetrains.mu.Unlock()

	originalCountTrainingRows := countTrainingRows
	t.Cleanup(func() {
		countTrainingRows = originalCountTrainingRows
		backgroundRetrains.mu.Lock()
		backgroundRetrains.states = make(map[string]*backgroundRetrainState)
		backgroundRetrains.mu.Unlock()
	})

	modelDir := t.TempDir()
	meta := Meta{
		TrainedAt:     "2024-01-01T00:00:00Z",
		JobCount:      50,
		SchemaVersion: ExpectedModelSchemaVersion,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "meta.json"), data, 0644); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	countTrainingRows = func(Config) (int, error) { return 120, nil }
	cfg := Config{ProjectPath: "/tmp/job-estimator", ModelDir: modelDir, RetrainInterval: 50}
	state := backgroundState(cfg)
	state.statusMu.Lock()
	state.running = true
	state.reason = "70 new completed jobs since last training"
	state.startedAt = time.Date(2026, time.April, 3, 12, 0, 0, 0, time.UTC)
	state.statusMu.Unlock()

	status := GetStatus(cfg)
	if !status.Ready {
		t.Fatal("expected compatible predictor to be ready")
	}
	if !status.Stale {
		t.Fatal("expected stale status")
	}
	if !status.BackgroundRebuildRunning {
		t.Fatal("expected background rebuild to be running")
	}
	if status.BackgroundRebuildReason == "" {
		t.Fatal("expected rebuild reason")
	}
	if status.NewCompletedJobs != 70 {
		t.Fatalf("NewCompletedJobs = %d, want 70", status.NewCompletedJobs)
	}
}

func TestGetStatusReportsSchemaBlock(t *testing.T) {
	backgroundRetrains.mu.Lock()
	backgroundRetrains.states = make(map[string]*backgroundRetrainState)
	backgroundRetrains.mu.Unlock()

	originalCountTrainingRows := countTrainingRows
	t.Cleanup(func() {
		countTrainingRows = originalCountTrainingRows
		backgroundRetrains.mu.Lock()
		backgroundRetrains.states = make(map[string]*backgroundRetrainState)
		backgroundRetrains.mu.Unlock()
	})

	modelDir := t.TempDir()
	meta := Meta{
		TrainedAt:     "2024-01-01T00:00:00Z",
		JobCount:      50,
		SchemaVersion: ExpectedModelSchemaVersion - 1,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "meta.json"), data, 0644); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	countTrainingRows = func(Config) (int, error) { return 60, nil }
	cfg := Config{ProjectPath: "/tmp/job-estimator", ModelDir: modelDir, RetrainInterval: 50}

	status := GetStatus(cfg)
	if status.Ready {
		t.Fatal("expected schema-incompatible predictor to be blocked")
	}
	if !status.SchemaIncompatible {
		t.Fatal("expected schema incompatibility")
	}
	if status.SchemaReason == "" {
		t.Fatal("expected schema reason")
	}
}

func TestGetStatusUsesEstimatorStatusWhenAvailable(t *testing.T) {
	originalRunStatusCLI := runStatusCLI
	originalCountTrainingRows := countTrainingRows
	t.Cleanup(func() {
		runStatusCLI = originalRunStatusCLI
		countTrainingRows = originalCountTrainingRows
	})

	runStatusCLI = func(Config) ([]byte, error) {
		return []byte(`{
			"ready": true,
			"model_present": true,
			"schema_compatible": true,
			"schema_reason": "",
			"schema_version": 3,
			"expected_schema_version": 3,
			"trained_at": "2026-04-03T00:00:00Z",
			"job_count": 120,
			"training_snapshot": {
				"total_rows": 150,
				"duration_training_rows": 120,
				"peak_rss_rows": 90,
				"max_gpu_mem_rows": 80,
				"max_run_id": 120,
				"max_created_at": 1000
			},
			"current_snapshot": {
				"total_rows": 162,
				"duration_training_rows": 132,
				"peak_rss_rows": 99,
				"max_gpu_mem_rows": 88,
				"max_run_id": 132,
				"max_created_at": 1200
			},
			"snapshot_changed": true,
			"duration_rows_since_training": 12,
			"retrain_interval": 10,
			"stale": true
		}`), nil
	}
	countTrainingRows = func(Config) (int, error) {
		t.Fatal("countTrainingRows should not be used when estimator status is available")
		return 0, nil
	}

	status := GetStatus(Config{ProjectPath: t.TempDir(), ModelDir: t.TempDir(), RetrainInterval: 10})
	if !status.Ready {
		t.Fatal("expected ready predictor status")
	}
	if status.JobCount != 120 {
		t.Fatalf("JobCount = %d, want 120", status.JobCount)
	}
	if status.CurrentJobCount != 132 {
		t.Fatalf("CurrentJobCount = %d, want 132", status.CurrentJobCount)
	}
	if status.NewCompletedJobs != 12 {
		t.Fatalf("NewCompletedJobs = %d, want 12", status.NewCompletedJobs)
	}
	if !status.Stale {
		t.Fatal("expected stale predictor status")
	}
}

func TestGetStatusCachesEstimatorStatusWithinTTL(t *testing.T) {
	backgroundRetrains.mu.Lock()
	backgroundRetrains.states = make(map[string]*backgroundRetrainState)
	backgroundRetrains.mu.Unlock()

	originalRunStatusCLI := runStatusCLI
	originalNowFunc := nowFunc
	originalTTL := predictorStatusCacheTTL
	originalFileTTL := predictorStatusFileCacheTTL
	t.Cleanup(func() {
		runStatusCLI = originalRunStatusCLI
		nowFunc = originalNowFunc
		predictorStatusCacheTTL = originalTTL
		predictorStatusFileCacheTTL = originalFileTTL
		backgroundRetrains.mu.Lock()
		backgroundRetrains.states = make(map[string]*backgroundRetrainState)
		backgroundRetrains.mu.Unlock()
	})
	// Disable file cache so this test can isolate the in-memory TTL behavior.
	predictorStatusFileCacheTTL = 0

	var calls atomic.Int32
	runStatusCLI = func(Config) ([]byte, error) {
		calls.Add(1)
		return []byte(`{
			"ready": true,
			"model_present": true,
			"schema_compatible": true,
			"schema_reason": "",
			"expected_schema_version": 3,
			"current_snapshot": {
				"total_rows": 10,
				"duration_training_rows": 10,
				"peak_rss_rows": 10,
				"max_gpu_mem_rows": 10
			},
			"duration_rows_since_training": 0,
			"retrain_interval": 50,
			"stale": false
		}`), nil
	}

	currentTime := time.Date(2026, time.April, 3, 10, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return currentTime }
	predictorStatusCacheTTL = 5 * time.Second

	cfg := Config{ProjectPath: t.TempDir(), ModelDir: t.TempDir()}
	_ = GetStatus(cfg)
	_ = GetStatus(cfg)
	if calls.Load() != 1 {
		t.Fatalf("runStatusCLI calls = %d, want 1 within cache TTL", calls.Load())
	}

	currentTime = currentTime.Add(6 * time.Second)
	_ = GetStatus(cfg)
	if calls.Load() != 2 {
		t.Fatalf("runStatusCLI calls = %d, want 2 after cache TTL", calls.Load())
	}
}

func TestPredictBatchCachesResultsAcrossCalls(t *testing.T) {
	clearPredictionCache()
	original := runPredictBatchCLI
	t.Cleanup(func() {
		runPredictBatchCLI = original
		clearPredictionCache()
	})

	calls := 0
	runPredictBatchCLI = func(_ Config, jobs []BatchJob) ([]byte, error) {
		calls++
		entries := make([]map[string]any, 0, len(jobs))
		for _, job := range jobs {
			entries = append(entries, map[string]any{
				"id": job.ID,
				"duration_s": map[string]any{
					"mean":  float64(3600),
					"std":   float64(60),
					"lower": float64(3500),
					"upper": float64(3700),
				},
			})
		}
		return json.Marshal(entries)
	}

	cfg := Config{ProjectPath: "/tmp/job-estimator", ModelDir: t.TempDir()}
	first, err := PredictBatch(cfg, []BatchJob{
		{ID: 1, Project: "p", GPUClass: "RTX 4090", Command: "python train.py"},
		{ID: 2, Project: "p", GPUClass: "RTX 4090", Command: "python train.py"},
	})
	if err != nil {
		t.Fatalf("PredictBatch first: %v", err)
	}
	second, err := PredictBatch(cfg, []BatchJob{
		{ID: 3, Project: "p", GPUClass: "RTX 4090", Command: "python train.py"},
	})
	if err != nil {
		t.Fatalf("PredictBatch second: %v", err)
	}

	if calls != 1 {
		t.Fatalf("runPredictBatchCLI calls = %d, want 1", calls)
	}
	if first[1] == nil || first[2] == nil || second[3] == nil {
		t.Fatalf("expected cached predictions for all ids, got %#v / %#v", first, second)
	}
}

func TestPredictBatchCacheSeparatesDurationQuantileRequests(t *testing.T) {
	clearPredictionCache()
	original := runPredictBatchCLI
	t.Cleanup(func() {
		runPredictBatchCLI = original
		clearPredictionCache()
	})

	calls := 0
	runPredictBatchCLI = func(_ Config, jobs []BatchJob) ([]byte, error) {
		calls++
		entries := make([]map[string]any, 0, len(jobs))
		for _, job := range jobs {
			entry := map[string]any{
				"id": job.ID,
				"duration_s": map[string]any{
					"mean":  float64(3600),
					"std":   float64(60),
					"lower": float64(3500),
					"upper": float64(3700),
				},
			}
			if len(job.DurationQuantiles) > 0 {
				entry["duration_quantiles"] = map[string]any{
					"quantiles": map[string]any{"0.5": float64(3600), "0.9": float64(7200)},
					"source":    "command",
					"n":         8,
					"mean_log":  math.Log(3600),
				}
			}
			entries = append(entries, entry)
		}
		return json.Marshal(entries)
	}

	cfg := Config{ProjectPath: "/tmp/job-estimator", ModelDir: t.TempDir()}
	if _, err := PredictBatch(cfg, []BatchJob{{ID: 1, Project: "p", GPUClass: "A100", Command: "python train.py"}}); err != nil {
		t.Fatalf("PredictBatch without quantiles: %v", err)
	}
	withQuantiles, err := PredictBatch(cfg, []BatchJob{{ID: 2, Project: "p", GPUClass: "A100", Command: "python train.py", DurationQuantiles: []float64{0.5, 0.9}}})
	if err != nil {
		t.Fatalf("PredictBatch with quantiles: %v", err)
	}

	if calls != 2 {
		t.Fatalf("runPredictBatchCLI calls = %d, want 2", calls)
	}
	q := withQuantiles[2].DurationQuantiles
	if q == nil {
		t.Fatal("expected duration quantiles")
	}
	got, ok := q.Quantile(0.7)
	if !ok || got != 5400 {
		t.Fatalf("Quantile(0.7) = %v, %v; want 5400, true", got, ok)
	}
}

func TestPredictedGPUMemGB(t *testing.T) {
	memGB, ok := PredictedGPUMemGB(&Prediction{Upper: 2050})
	if !ok {
		t.Fatal("expected prediction to convert")
	}
	if memGB != 3 {
		t.Fatalf("PredictedGPUMemGB upper=2050MiB = %d, want 3", memGB)
	}
}

func TestPredictedGPUMemCeilingGB(t *testing.T) {
	tests := []struct {
		name   string
		upper  float64 // MiB
		wantGB int
		wantOK bool
	}{
		{"nil prediction", 0, 0, false},
		{"20GB upper snaps to 24", 20 * 1024, 24, true},
		{"23.5GB upper snaps to 24", 23.5 * 1024, 24, true},
		{"24GB upper snaps to 24", 24 * 1024, 24, true},
		{"25GB upper snaps to 48", 25 * 1024, 48, true},
		{"45GB upper snaps to 48", 45 * 1024, 48, true},
		{"50GB upper snaps to 80", 50 * 1024, 80, true},
		{"10GB upper snaps to 12", 10 * 1024, 12, true},
		{"150GB upper has no ceiling", 150 * 1024, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p *Prediction
			if tt.upper > 0 {
				p = &Prediction{Upper: tt.upper}
			}
			got, ok := PredictedGPUMemCeilingGB(p)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.wantGB {
				t.Fatalf("ceiling = %d, want %d", got, tt.wantGB)
			}
		})
	}
}

func TestResolveGPUMem_ReturnsCeiling(t *testing.T) {
	original := predictFunc
	t.Cleanup(func() { predictFunc = original })

	cfg := Config{ProjectPath: "/tmp/job-estimator"}

	t.Run("prediction yields ceiling", func(t *testing.T) {
		predictFunc = func(Config, string, string, string, string) (*Result, error) {
			return &Result{MaxGPUMemMiB: &Prediction{Upper: 20 * 1024}}, nil // 20GB
		}
		floor, ceiling, _ := ResolveGPUMem(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 16, 0, 0)
		if floor == nil || *floor < 20 {
			t.Fatalf("floor = %v, want >= 20", floor)
		}
		if ceiling == nil {
			t.Fatal("ceiling should not be nil for 20GB prediction")
		}
		if *ceiling != 24 {
			t.Fatalf("ceiling = %d, want 24", *ceiling)
		}
	})

	t.Run("explicit value preserves predictor ceiling", func(t *testing.T) {
		predictFunc = func(Config, string, string, string, string) (*Result, error) {
			return &Result{MaxGPUMemMiB: &Prediction{Upper: 20 * 1024}}, nil // 20GB
		}
		explicit := 48
		floor, ceiling, predicted := ResolveGPUMem(cfg, &explicit, true, "host-a", "proj", "a100", "python train.py", 20, 0, 0)
		if floor == nil || *floor != explicit {
			t.Fatalf("floor = %v, want %d", floor, explicit)
		}
		if predicted {
			t.Fatal("predicted should be false when explicit floor wins")
		}
		if ceiling == nil {
			t.Fatal("ceiling should still be present with explicit floor")
		}
		if *ceiling != explicit {
			t.Fatalf("ceiling = %d, want %d", *ceiling, explicit)
		}
	})

	t.Run("ceiling at least as large as floor", func(t *testing.T) {
		predictFunc = func(Config, string, string, string, string) (*Result, error) {
			return &Result{MaxGPUMemMiB: &Prediction{Upper: 10 * 1024}}, nil // 10GB
		}
		// fallbackGB=16 is larger than prediction, so floor=16, ceiling should be >= 16
		floor, ceiling, _ := ResolveGPUMem(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 16, 0, 0)
		if floor == nil || *floor != 16 {
			t.Fatalf("floor = %v, want 16", floor)
		}
		if ceiling == nil {
			t.Fatal("ceiling should not be nil")
		}
		if *ceiling < *floor {
			t.Fatalf("ceiling %d < floor %d", *ceiling, *floor)
		}
	})
}

func TestResolveGPUMemGB(t *testing.T) {
	original := predictFunc
	t.Cleanup(func() { predictFunc = original })

	cfg := Config{ProjectPath: "/tmp/job-estimator"}

	t.Run("explicit value wins", func(t *testing.T) {
		explicit := 48
		predictFunc = func(Config, string, string, string, string) (*Result, error) {
			return nil, fmt.Errorf("should not be called")
		}
		got, estimated := ResolveGPUMemGB(cfg, &explicit, true, "host-a", "proj", "a100", "python train.py", 20, 0, 0)
		if got == nil || *got != 48 {
			t.Fatalf("ResolveGPUMemGB explicit = %v, want 48", got)
		}
		if estimated {
			t.Fatalf("expected explicit reservation to report estimated=false")
		}
	})

	t.Run("predictor upper bound wins when above fallback", func(t *testing.T) {
		predictFunc = func(Config, string, string, string, string) (*Result, error) {
			return &Result{MaxGPUMemMiB: &Prediction{Upper: 25 * 1024}}, nil // 25GB
		}
		got, estimated := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 20, 0, 0)
		if got == nil || *got != 25 {
			t.Fatalf("ResolveGPUMemGB predicted = %v, want 25", got)
		}
		if !estimated {
			t.Fatalf("expected predictor-backed reservation to report estimated=true")
		}
	})

	t.Run("prediction fallback uses default reservation", func(t *testing.T) {
		predictFunc = func(Config, string, string, string, string) (*Result, error) {
			return &Result{}, nil
		}
		got, estimated := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 20, 0, 0)
		if got == nil || *got != 20 {
			t.Fatalf("ResolveGPUMemGB fallback = %v, want 20", got)
		}
		if estimated {
			t.Fatalf("expected fallback reservation to report estimated=false")
		}
	})

	t.Run("oom floor raises minimum", func(t *testing.T) {
		predictFunc = func(Config, string, string, string, string) (*Result, error) {
			return &Result{}, nil // no prediction
		}
		got, estimated := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 20, 25, 0)
		if got == nil || *got != 25 {
			t.Fatalf("ResolveGPUMemGB with oom floor = %v, want 25", got)
		}
		if estimated {
			t.Fatalf("expected oom floor reservation to report estimated=false")
		}
	})

	t.Run("predictor wins over oom floor when higher", func(t *testing.T) {
		predictFunc = func(Config, string, string, string, string) (*Result, error) {
			return &Result{MaxGPUMemMiB: &Prediction{Upper: 50 * 1024}}, nil // 50GB
		}
		got, estimated := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 20, 25, 0)
		if got == nil || *got != 50 {
			t.Fatalf("ResolveGPUMemGB predictor>floor = %v, want 50", got)
		}
		if !estimated {
			t.Fatalf("expected predictor to win and report estimated=true")
		}
	})

	t.Run("cpu jobs keep nil reservation", func(t *testing.T) {
		predictFunc = func(Config, string, string, string, string) (*Result, error) {
			return nil, fmt.Errorf("should not be called")
		}
		got, estimated := ResolveGPUMemGB(cfg, nil, false, "", "proj", "", "echo hi", 20, 0, 0)
		if got != nil {
			t.Fatalf("ResolveGPUMemGB cpu job = %v, want nil", *got)
		}
		if estimated {
			t.Fatalf("expected cpu job reservation to report estimated=false")
		}
	})
}

// TestResolveGPUMem_ClassMemCeiling is the wj3135 regression: a "--gpu t4" job
// with no --gpu-mem must not inherit the 20GB blanket default (no 16GB T4 offer
// can satisfy gpu_ram>=20). The fallback default is clamped to the named class
// ceiling, but a real demand signal (prediction or OOM floor) above the ceiling
// is left intact so the too-small pin surfaces as "no offers" pre-launch.
func TestResolveGPUMem_ClassMemCeiling(t *testing.T) {
	original := predictFunc
	t.Cleanup(func() { predictFunc = original })
	cfg := Config{ProjectPath: "/tmp/job-estimator"}

	noPrediction := func(Config, string, string, string, string) (*Result, error) {
		return &Result{}, nil
	}
	predict := func(gb int) func(Config, string, string, string, string) (*Result, error) {
		return func(Config, string, string, string, string) (*Result, error) {
			return &Result{MaxGPUMemMiB: &Prediction{Upper: float64(gb) * 1024}}, nil
		}
	}

	t.Run("fallback default clamps to ceiling", func(t *testing.T) {
		predictFunc = noPrediction
		got, _ := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "t4", "python infer.py", 20, 0, 16)
		if got == nil || *got != 16 {
			t.Fatalf("clamped fallback = %v, want 16", got)
		}
	})

	t.Run("small prediction under ceiling still clamps fallback", func(t *testing.T) {
		predictFunc = predict(4) // gpt2-sized; loses max to fallback 20
		got, _ := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "t4", "python infer.py", 20, 0, 16)
		if got == nil || *got != 16 {
			t.Fatalf("clamped fallback with small prediction = %v, want 16", got)
		}
	})

	t.Run("ceiling at or above default is a no-op", func(t *testing.T) {
		predictFunc = noPrediction
		got, _ := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 20, 0, 80)
		if got == nil || *got != 20 {
			t.Fatalf("no-clamp default = %v, want 20", got)
		}
	})

	t.Run("prediction above ceiling is not clamped", func(t *testing.T) {
		predictFunc = predict(18) // exceeds the 16GB ceiling
		got, _ := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "t4", "python train.py", 20, 0, 16)
		if got == nil || *got != 20 {
			t.Fatalf("prediction-above-ceiling = %v, want 20 (surfaces as no-offers)", got)
		}
	})

	t.Run("oom floor above ceiling is not clamped", func(t *testing.T) {
		predictFunc = noPrediction
		// fallback below the OOM floor so the floor wins the max and the
		// above-ceiling guard is the thing under test.
		got, _ := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "t4", "python train.py", 10, 18, 16)
		if got == nil || *got != 18 {
			t.Fatalf("oom-floor-above-ceiling = %v, want 18 (surfaces as no-offers)", got)
		}
	})

	t.Run("explicit request is never clamped", func(t *testing.T) {
		predictFunc = noPrediction
		explicit := 24
		got, _ := ResolveGPUMemGB(cfg, &explicit, true, "host-a", "proj", "t4", "python train.py", 20, 0, 16)
		if got == nil || *got != 24 {
			t.Fatalf("explicit = %v, want 24 (left to fail loudly)", got)
		}
	})

	t.Run("no ceiling known is a no-op", func(t *testing.T) {
		predictFunc = noPrediction
		got, _ := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "nvidia", "python train.py", 20, 0, 0)
		if got == nil || *got != 20 {
			t.Fatalf("no-ceiling = %v, want 20", got)
		}
	})
}
