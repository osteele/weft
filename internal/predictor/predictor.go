// Package predictor shells out to the job-predictor Python CLI to train
// models and predict job duration, peak RSS, and peak GPU memory.
package predictor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Config controls how the predictor finds and invokes job-predictor.
type Config struct {
	// ProjectPath is the path to the job-predictor Python project checkout.
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
	Mean  float64 `json:"mean"`
	Std   float64 `json:"std"`
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// Result holds predictions for all targets.
type Result struct {
	DurationS    *Prediction `json:"duration_s"`
	PeakRSSKB    *Prediction `json:"peak_rss_kb"`
	MaxGPUMemMiB *Prediction `json:"max_gpu_mem_mib"`
}

// Meta is the sidecar metadata written alongside trained models.
type Meta struct {
	TrainedAt string         `json:"trained_at"`
	JobCount  int            `json:"job_count"`
	DBPaths   []string       `json:"db_paths"`
	Models    map[string]any `json:"models"`
}

// Configured returns true if the predictor has a project path set.
func (c *Config) Configured() bool {
	return c.ProjectPath != ""
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

// NeedsRetrain returns true if models are stale or missing.
// It compares the job_count in meta.json against currentJobCount.
func NeedsRetrain(cfg Config, currentJobCount int) bool {
	meta, err := ReadMeta(cfg)
	if err != nil {
		return true // Missing or corrupt meta → retrain
	}
	return currentJobCount-meta.JobCount >= cfg.retrainInterval()
}

// Train shells out to job-predictor train with the configured DB paths.
func Train(cfg Config) error {
	if cfg.ProjectPath == "" {
		return fmt.Errorf("predictor: project_path not configured")
	}

	args := []string{"run", "--project", cfg.ProjectPath, "job-predictor", "train"}
	for _, db := range cfg.DBPaths {
		args = append(args, "--db", db)
	}
	args = append(args, "--model-dir", cfg.modelDir())

	cmd := exec.Command("uv", args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Predict shells out to job-predictor predict and parses the JSON result.
func Predict(cfg Config, host, project, gpuClass, command string) (*Result, error) {
	if cfg.ProjectPath == "" {
		return nil, fmt.Errorf("predictor: project_path not configured")
	}

	args := []string{
		"run", "--project", cfg.ProjectPath, "job-predictor", "predict",
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

	var result Result
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("predictor: parse output: %w", err)
	}
	return &result, nil
}

// EnsureAndPredict retrains if stale, then predicts.
// Errors are non-fatal: returns nil result if prediction fails.
func EnsureAndPredict(cfg Config, currentJobCount int, host, project, gpuClass, command string) *Result {
	if NeedsRetrain(cfg, currentJobCount) {
		if err := Train(cfg); err != nil {
			return nil
		}
	}
	result, err := Predict(cfg, host, project, gpuClass, command)
	if err != nil {
		return nil
	}
	return result
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
	divisor := 1.0
	switch unit {
	case "GiB":
		divisor = 1024
	case "MiB":
		divisor = 1
	case "GB":
		divisor = 1000
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
