package campaign

import (
	"fmt"
	"strings"
)

// BootstrapManifest describes everything an instance needs to self-start.
type BootstrapManifest struct {
	AgentR2Key    string          // R2 key for the agent binary
	Sources       []SourceMapping // source tarballs to extract
	WrapperScript string          // pre-generated wrapper script content
	WorkspacePath string          // e.g. "/workspace/"
	DonorMode     bool            // download caches and write ready marker, no wrapper
	HFModels      []string        // HF model IDs to pre-download (donor mode only)
	DonorID       string          // provider instance ID (for R2 ready marker key)
	DBInstanceID  int64           // DB cloud_instances.id for R2 stage markers
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
		"echo '%s' | rclone rcat \"r2:$R2_BUCKET/bootstrap/%d/stage\"\n",
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

	// Download and install agent binary
	b.WriteString("# Install agent binary\n")
	b.WriteString(fmt.Sprintf(
		"rclone copyto \"r2:$R2_BUCKET/%s\" /usr/local/bin/weft-agent\n",
		manifest.AgentR2Key,
	))
	b.WriteString("chmod +x /usr/local/bin/weft-agent\n")
	writeStageMarker(&b, manifest.DBInstanceID, "agent_installed")
	b.WriteString("\n")

	// Download and extract source tarballs
	for _, src := range manifest.Sources {
		b.WriteString(fmt.Sprintf("# Extract source to %s\n", src.RemoteDir))
		b.WriteString(fmt.Sprintf("mkdir -p %q\n", src.RemoteDir))
		b.WriteString(fmt.Sprintf(
			"rclone copyto \"r2:$R2_BUCKET/%s\" /tmp/src.tar.gz && tar xzf /tmp/src.tar.gz -C %q && rm -f /tmp/src.tar.gz\n",
			src.R2Key, src.RemoteDir,
		))
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

// generateDonorBootstrapTail generates the donor-specific portion:
// runs uv sync, downloads HF models, writes ready marker to R2.
func generateDonorBootstrapTail(b *strings.Builder, manifest BootstrapManifest) {
	// Run uv sync in each source directory
	for _, src := range manifest.Sources {
		b.WriteString(fmt.Sprintf("# Run uv sync in %s\n", src.RemoteDir))
		b.WriteString(fmt.Sprintf("cd %q && uv sync 2>&1 || echo 'uv sync failed in %s'\n\n", src.RemoteDir, src.RemoteDir))
	}
	writeStageMarker(b, manifest.DBInstanceID, "deps_installed")

	// Download each HF model
	if len(manifest.HFModels) > 0 {
		b.WriteString("# Download HF models\n")
		b.WriteString("_hf_done=0\n")
		b.WriteString(fmt.Sprintf("_hf_total=%d\n", len(manifest.HFModels)))
		for _, model := range manifest.HFModels {
			b.WriteString(fmt.Sprintf("echo 'Downloading HF model: %s'\n", model))
			b.WriteString(fmt.Sprintf("huggingface-cli download %q 2>&1 || echo 'Failed to download %s'\n", model, model))
			b.WriteString("_hf_done=$((_hf_done + 1))\n")
			writeStageMarker(b, manifest.DBInstanceID, "downloading_models:${_hf_done}/${_hf_total}")
		}
		b.WriteString("\n")

		// Repair broken HF symlinks (known issue with huggingface_hub cache layout)
		b.WriteString("# Repair broken HF symlinks\n")
		b.WriteString(`find /root/.cache/huggingface -type l ! -exec test -e {} \; -delete 2>/dev/null || true` + "\n\n")
	}

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
// writes and starts the wrapper script.
func generateWorkerBootstrapTail(b *strings.Builder, manifest BootstrapManifest) {
	// Write wrapper script
	wsPath := manifest.WorkspacePath
	if wsPath == "" {
		wsPath = "/workspace/"
	}
	wrapperPath := fmt.Sprintf("%s.weft-campaign.sh", strings.TrimRight(wsPath, "/"))
	b.WriteString("# Write wrapper script\n")
	b.WriteString(fmt.Sprintf("cat > %s << 'WRAPPER_EOF'\n", wrapperPath))
	b.WriteString(manifest.WrapperScript)
	b.WriteString("WRAPPER_EOF\n")
	b.WriteString(fmt.Sprintf("chmod +x %s\n\n", wrapperPath))

	// Start wrapper via nohup
	b.WriteString("# Start wrapper\n")
	b.WriteString(fmt.Sprintf("nohup bash %s </dev/null >>/tmp/wrapper.log 2>&1 &\n", wrapperPath))
}
