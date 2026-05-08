package campaign

import (
	"fmt"
	"strings"
)

// BootstrapManifest describes everything an instance needs to self-start.
type BootstrapManifest struct {
	AgentR2Key         string          // R2 key for the agent binary
	Sources            []SourceMapping // source tarballs to extract
	Image              string          // Docker image for deciding setup optimizations
	DonorMode          bool            // download caches and write ready marker, no wrapper
	HFModels           []string        // HF model IDs to pre-download before jobs run
	HFDatasets         []string        // HF dataset IDs to pre-download before jobs run
	DonorID            string          // provider instance ID (for R2 ready marker key)
	DBInstanceID       int64           // DB cloud_instances.id for R2 stage markers
	MaxTimeSeconds     int             // instance time budget (0 = unlimited)
	GracePeriodSeconds int             // grace period after job failure (0 = disabled)
	AgentForeground    bool            // run agent as the container command instead of backgrounding it
}

// SourceMapping maps an R2 key to a target directory on the instance.
type SourceMapping struct {
	R2Key     string // e.g. "sources/sha256abc.tar.gz"
	RemoteDir string // e.g. "/workspace/my-project"
}

// writeStageMarker emits a bash command to write a bootstrap stage marker to R2.
func writeStageMarker(b *strings.Builder, instanceID int64, stage string) {
	if instanceID == 0 {
		return
	}
	b.WriteString(fmt.Sprintf(
		"echo \"%s\" | rclone rcat \"r2:$R2_BUCKET/bootstrap/%d/stage\"\n",
		stage, instanceID,
	))
}

// GenerateBootstrapScript produces a bash script that downloads assets from R2
// and starts the wrapper. The script uses R2_BUCKET env var (set via --env).
func GenerateBootstrapScript(manifest BootstrapManifest) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n\n")

	// Ensure uv and rclone are in PATH
	b.WriteString("export PATH=\"$HOME/.local/bin:$PATH\"\n\n")
	b.WriteString("# Keep large dependency and model caches on the rented data volume.\n")
	b.WriteString("mkdir -p /workspace/.cache/huggingface /workspace/.cache/uv\n")
	b.WriteString("export XDG_CACHE_HOME=/workspace/.cache\n")
	b.WriteString("export HF_HOME=/workspace/.cache/huggingface\n")
	b.WriteString("export HF_HUB_CACHE=/workspace/.cache/huggingface/hub\n")
	b.WriteString("export HUGGINGFACE_HUB_CACHE=/workspace/.cache/huggingface/hub\n")
	b.WriteString("export UV_CACHE_DIR=/workspace/.cache/uv\n\n")
	writeFailureTrap(&b, manifest.DBInstanceID)
	writeHFDownloadHelpers(&b)

	// Download and install agent binary
	b.WriteString("# Install agent binary\n")
	writeStageMarker(&b, manifest.DBInstanceID, "agent_installing")
	b.WriteString(fmt.Sprintf(
		"rclone copyto \"r2:$R2_BUCKET/%s\" /usr/local/bin/weft-agent\n",
		manifest.AgentR2Key,
	))
	b.WriteString("chmod +x /usr/local/bin/weft-agent\n")
	writeStageMarker(&b, manifest.DBInstanceID, "agent_installed")
	b.WriteString("\n")

	// Download and extract source tarballs
	sourceCount := len(manifest.Sources)
	if sourceCount > 0 {
		writeStageMarker(&b, manifest.DBInstanceID, fmt.Sprintf("sources_extracting:0/%d", sourceCount))
	}
	for i, src := range manifest.Sources {
		b.WriteString(fmt.Sprintf("# Extract source to %s\n", src.RemoteDir))
		b.WriteString(fmt.Sprintf("mkdir -p %q\n", src.RemoteDir))
		b.WriteString(fmt.Sprintf(
			"rclone copyto \"r2:$R2_BUCKET/%s\" /tmp/src.tar.gz && tar xzf /tmp/src.tar.gz -C %q && rm -f /tmp/src.tar.gz\n",
			src.R2Key, src.RemoteDir,
		))
		writeStageMarker(&b, manifest.DBInstanceID, fmt.Sprintf("sources_extracting:%d/%d", i+1, sourceCount))
	}
	writeStageMarker(&b, manifest.DBInstanceID, "sources_extracted")
	b.WriteString("\n")

	if manifest.DonorMode {
		generateDonorBootstrapTail(&b, manifest)
	} else {
		writeStageMarker(&b, manifest.DBInstanceID, "starting_jobs")
		generateWorkerBootstrapTail(&b, manifest)
	}

	return b.String()
}

// writeHFDownloads emits bash commands to pre-download declared HF assets.
func writeHFDownloads(b *strings.Builder, models, datasets []string, instanceID int64) {
	total := len(models) + len(datasets)
	if total == 0 {
		return
	}
	b.WriteString("# Download HF assets\n")
	writeStageMarker(b, instanceID, "hf_tool_installing")
	b.WriteString("ensure_hf_download_tool\n")
	writeStageMarker(b, instanceID, "hf_tool_installed")
	b.WriteString("_hf_done=0\n")
	b.WriteString(fmt.Sprintf("_hf_total=%d\n", total))
	writeStageMarker(b, instanceID, "downloading_hf:${_hf_done}/${_hf_total}")
	for _, model := range models {
		b.WriteString(fmt.Sprintf("echo 'Downloading HF model: %s'\n", model))
		b.WriteString(fmt.Sprintf("hf_download model %q\n", model))
		b.WriteString("_hf_done=$((_hf_done + 1))\n")
		writeStageMarker(b, instanceID, "downloading_hf:${_hf_done}/${_hf_total}")
	}
	for _, dataset := range datasets {
		b.WriteString(fmt.Sprintf("echo 'Downloading HF dataset: %s'\n", dataset))
		b.WriteString(fmt.Sprintf("hf_download dataset %q\n", dataset))
		b.WriteString("_hf_done=$((_hf_done + 1))\n")
		writeStageMarker(b, instanceID, "downloading_hf:${_hf_done}/${_hf_total}")
	}
	b.WriteString("\n")

	// Repair broken HF symlinks (known issue with huggingface_hub cache layout)
	b.WriteString("# Repair broken HF symlinks\n")
	b.WriteString(`find "${HF_HOME:-/root/.cache/huggingface}" -type l ! -exec test -e {} \; -delete 2>/dev/null || true` + "\n\n")
}

func writeFailureTrap(b *strings.Builder, instanceID int64) {
	if instanceID == 0 {
		return
	}
	b.WriteString(fmt.Sprintf(
		"trap '_weft_rc=$?; echo \"failed:${_weft_rc}\" | rclone rcat \"r2:$R2_BUCKET/bootstrap/%d/stage\" >/dev/null 2>&1 || true; exit ${_weft_rc}' ERR\n\n",
		instanceID,
	))
}

func writeHFDownloadHelpers(b *strings.Builder) {
	b.WriteString(`ensure_hf_download_tool() {
  if command -v hf >/dev/null 2>&1 || command -v huggingface-cli >/dev/null 2>&1; then
    return 0
  fi
  if command -v uv >/dev/null 2>&1; then
    uv tool install 'huggingface-hub[hf_xet]' >/dev/null
  elif command -v python3 >/dev/null 2>&1; then
    python3 -m pip install --quiet 'huggingface-hub[hf_xet]'
  else
    echo 'no uv or python3 available to install huggingface-hub' >&2
    return 127
  fi
}

hf_download() {
  _repo_type="$1"
  _repo_id="$2"
  if command -v hf >/dev/null 2>&1; then
    hf download --repo-type "$_repo_type" "$_repo_id" 2>&1
  elif command -v huggingface-cli >/dev/null 2>&1; then
    huggingface-cli download --repo-type "$_repo_type" "$_repo_id" 2>&1
  else
    python3 -c 'import sys; from huggingface_hub import snapshot_download; snapshot_download(repo_id=sys.argv[1], repo_type=sys.argv[2])' "$_repo_id" "$_repo_type"
  fi
}

`)
}

// generateDonorBootstrapTail generates the donor-specific portion:
// runs uv sync, downloads HF models, writes ready marker to R2.
func generateDonorBootstrapTail(b *strings.Builder, manifest BootstrapManifest) {
	// Run uv sync in each source directory
	writeStageMarker(b, manifest.DBInstanceID, "deps_installing")
	if isPyTorchImage(manifest.Image) {
		b.WriteString("# Reuse torch packages from the PyTorch base image when possible\n")
		b.WriteString("if [ -x /opt/conda/bin/python ]; then\n")
		b.WriteString("  export UV_PYTHON=/opt/conda/bin/python\n")
		b.WriteString("fi\n\n")
	}
	for _, src := range manifest.Sources {
		b.WriteString(fmt.Sprintf("# Run uv sync in %s\n", src.RemoteDir))
		if isPyTorchImage(manifest.Image) {
			b.WriteString(fmt.Sprintf("cd %q && ([ -d .venv ] || ([ -n \"${UV_PYTHON:-}\" ] && \"$UV_PYTHON\" -m venv --system-site-packages .venv)) && uv sync --no-install-package torch --no-install-package torchaudio --no-install-package torchvision 2>&1 || echo 'uv sync failed in %s'\n\n", src.RemoteDir, src.RemoteDir))
		} else {
			b.WriteString(fmt.Sprintf("cd %q && uv sync 2>&1 || echo 'uv sync failed in %s'\n\n", src.RemoteDir, src.RemoteDir))
		}
	}
	writeStageMarker(b, manifest.DBInstanceID, "deps_installed")

	writeHFDownloads(b, manifest.HFModels, manifest.HFDatasets, manifest.DBInstanceID)

	writeStageMarker(b, manifest.DBInstanceID, "ready")

	// Write ready marker to R2
	b.WriteString("# Signal readiness to coordinator\n")
	b.WriteString(fmt.Sprintf(
		"echo 'ready' | rclone rcat \"r2:$R2_BUCKET/donor/%s/.ready\"\n",
		manifest.DonorID,
	))
	b.WriteString("echo 'Donor ready'\n")
}

// generateWorkerBootstrapTail generates the worker-specific portion:
// launches weft-agent run-campaign via nohup.
func generateWorkerBootstrapTail(b *strings.Builder, manifest BootstrapManifest) {
	b.WriteString("# Launch campaign agent\n")
	writeStageMarker(b, manifest.DBInstanceID, "agent_starting")
	if manifest.AgentForeground {
		b.WriteString("exec ")
	} else {
		b.WriteString("nohup ")
	}
	b.WriteString(fmt.Sprintf("weft-agent run-campaign"+
		" --r2-bucket=$R2_BUCKET"+
		" --instance-id=%d",
		manifest.DBInstanceID))

	if manifest.MaxTimeSeconds > 0 {
		b.WriteString(fmt.Sprintf(" --max-time=%ds", manifest.MaxTimeSeconds))
	}
	if manifest.GracePeriodSeconds > 0 {
		b.WriteString(fmt.Sprintf(" --grace-period=%ds", manifest.GracePeriodSeconds))
	}

	b.WriteString(" </dev/null >>/tmp/wrapper.log 2>&1")
	if !manifest.AgentForeground {
		b.WriteString(" &")
	}
	b.WriteString("\n")
}
