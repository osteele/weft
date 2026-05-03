package campaign

import (
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/workdir"
)

const (
	uvRuntimeDiskGB        = 8
	pipRuntimeDiskGB       = 8
	cudaRuntimeDiskGB      = 14
	vllmRuntimeDiskGB      = 18
	flashAttnRuntimeDiskGB = 12
)

// EstimateRuntimeDiskGB estimates transient runtime disk beyond declared data
// inputs and base environment overhead: wheel caches, build temp dirs, and
// setup-time package installs.
func EstimateRuntimeDiskGB(job *db.Job) int {
	if job == nil {
		return 0
	}
	if job.Metadata != nil && job.Metadata.Disk != nil && job.Metadata.Disk.RuntimeDiskGB > 0 {
		return job.Metadata.Disk.RuntimeDiskGB
	}

	command := strings.ToLower(job.EffectiveCommand())
	localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	diskGB := 0
	if strings.Contains(command, "uv sync") || strings.Contains(command, "uv run") {
		diskGB += uvRuntimeDiskGB
	}
	if strings.Contains(command, "pip install") || strings.Contains(command, "python -m pip") {
		diskGB += pipRuntimeDiskGB
	}
	if commandMentionsAny(command, "vllm", "nvidia-cublas", "nvidia-cudnn") || commandProjectHasCUDA(command, localDir) {
		diskGB += vllmRuntimeDiskGB
	} else if commandMentionsAny(command, "torch", "xformers", "jax", "tensorflow", "nvidia-") {
		diskGB += cudaRuntimeDiskGB
	}
	if commandMentionsAny(command, "flash-attn", "flash_attn") {
		diskGB += flashAttnRuntimeDiskGB
	}
	return diskGB
}

func groupRuntimeDiskGB(group InstanceGroup) int {
	total := 0
	seen := make(map[string]bool)
	for _, job := range group.Jobs {
		if job == nil {
			continue
		}
		key := job.Project + "\x00" + job.EffectiveCommand()
		if seen[key] {
			continue
		}
		seen[key] = true
		total += EstimateRuntimeDiskGB(job)
	}
	return total
}

func groupDiskFloorGB(group InstanceGroup) int {
	floor := 0
	for _, job := range group.Jobs {
		if job == nil || job.Metadata == nil || job.Metadata.Disk == nil {
			continue
		}
		if job.Metadata.Disk.DiskGB > floor {
			floor = job.Metadata.Disk.DiskGB
		}
	}
	return floor
}

func commandMentionsAny(command string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(command, needle) {
			return true
		}
	}
	return false
}

func commandProjectHasCUDA(command, localDir string) bool {
	projectDir := uvProjectDir(command, localDir)
	if projectDir == "" {
		return false
	}
	return hasCUDAInPyproject(filepath.Join(projectDir, "pyproject.toml"))
}

func uvProjectDir(command, localDir string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		if field != "--project" || i+1 >= len(fields) {
			continue
		}
		project := strings.Trim(fields[i+1], `"'`)
		if filepath.IsAbs(project) {
			return project
		}
		if localDir != "" {
			return filepath.Join(localDir, project)
		}
	}
	return localDir
}
