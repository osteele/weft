package predictor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
			"duration_metadata": {"source":"learned+analytical","confidence":0.625,"feasible":true,"analytical_duration_s":90.0,"analytical_peak_memory_mib":2048.0},
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
		floor, ceiling, _ := ResolveGPUMem(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 16, 0)
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
		floor, ceiling, predicted := ResolveGPUMem(cfg, &explicit, true, "host-a", "proj", "a100", "python train.py", 20, 0)
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
		floor, ceiling, _ := ResolveGPUMem(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 16, 0)
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
		got, estimated := ResolveGPUMemGB(cfg, &explicit, true, "host-a", "proj", "a100", "python train.py", 20, 0)
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
		got, estimated := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 20, 0)
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
		got, estimated := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 20, 0)
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
		got, estimated := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 20, 25)
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
		got, estimated := ResolveGPUMemGB(cfg, nil, true, "host-a", "proj", "a100", "python train.py", 20, 25)
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
		got, estimated := ResolveGPUMemGB(cfg, nil, false, "", "proj", "", "echo hi", 20, 0)
		if got != nil {
			t.Fatalf("ResolveGPUMemGB cpu job = %v, want nil", *got)
		}
		if estimated {
			t.Fatalf("expected cpu job reservation to report estimated=false")
		}
	})
}
