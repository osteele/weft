package campaign

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/dataplane"
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
	LocalDir  string // local source directory, used to read the project uv.lock
	Blobs     []dataplane.SourceBlob
}

// writeStageMarker emits a bash command to write a bootstrap stage marker
// through the _weft_mark_stage helper, which retries the rcat and swallows
// failure so a transient R2 5xx cannot abort the bootstrap under set -e.
// Markers are advisory; see lab-notebook EXP-021.
func writeStageMarker(b *strings.Builder, instanceID int64, stage string) {
	if instanceID == 0 {
		return
	}
	b.WriteString(fmt.Sprintf(
		"_weft_mark_stage %d %q\n",
		instanceID, stage,
	))
}

// writeStageHelper emits the bash function that all stage-marker writes go
// through. It retries a small number of times to ride out brief R2 hiccups,
// then unconditionally returns success so the caller cannot abort the
// bootstrap on a marker-write failure.
func writeStageHelper(b *strings.Builder) {
	b.WriteString(`_weft_mark_stage() {
  # Best-effort bootstrap stage marker: retry briefly, then give up cleanly.
  # Markers are advisory; bootstrap must continue even if R2 is flaking.
  local _id="$1" _stage="$2" _i
  for _i in 1 2 3 4 5; do
    if echo "${_stage}" | rclone rcat "r2:${R2_BUCKET}/bootstrap/${_id}/stage" 2>/dev/null; then
      return 0
    fi
    sleep 2
  done
  echo "weft: failed to write stage marker '${_stage}' after retries (continuing)" >&2
  return 0
}

`)
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
	writeStageHelper(&b)
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
		writeSourceBlobs(&b, src)
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

// writeSourceBlobs emits bash commands to batch download, place, and verify
// content-addressed source blobs for a source mapping.
func writeSourceBlobs(b *strings.Builder, src SourceMapping) {
	if len(src.Blobs) == 0 {
		return
	}
	b.WriteString(fmt.Sprintf("# Materialize source blobs for %s\n", src.RemoteDir))
	stagePattern := filepath.ToSlash(filepath.Join(src.RemoteDir, ".weft-blobs.XXXXXX"))
	b.WriteString(fmt.Sprintf("_blob_stage=$(mktemp -d %q)\n", stagePattern))
	b.WriteString("trap 'rm -rf \"${_blob_stage:-}\"' EXIT\n")

	// Count occurrences of each key to use `mv` on the last reference
	keyCount := make(map[string]int, len(src.Blobs))
	for _, blob := range src.Blobs {
		keyCount[blob.R2Key]++
	}

	// Deduplicate R2 keys for batch download
	seenKeys := make(map[string]struct{}, len(src.Blobs))
	var uniqueKeys []string
	for _, blob := range src.Blobs {
		if _, ok := seenKeys[blob.R2Key]; !ok {
			seenKeys[blob.R2Key] = struct{}{}
			uniqueKeys = append(uniqueKeys, blob.R2Key)
		}
	}

	b.WriteString("cat <<'EOF' > \"$_blob_stage/files-from.txt\"\n")
	for _, key := range uniqueKeys {
		b.WriteString(key)
		b.WriteString("\n")
	}
	b.WriteString("EOF\n")

	b.WriteString("if ! rclone copy \"r2:$R2_BUCKET\" \"$_blob_stage\" --files-from \"$_blob_stage/files-from.txt\"; then\n")
	b.WriteString("  echo \"weft: batch blob copy failed; falling back to individual download\" >&2\n")
	b.WriteString("  while IFS= read -r _blob_key; do\n")
	b.WriteString("    [ -n \"$_blob_key\" ] || continue\n")
	b.WriteString("    mkdir -p \"$_blob_stage/$(dirname \"$_blob_key\")\"\n")
	b.WriteString("    rclone copyto \"r2:$R2_BUCKET/$_blob_key\" \"$_blob_stage/$_blob_key\"\n")
	b.WriteString("  done < \"$_blob_stage/files-from.txt\"\n")
	b.WriteString("fi\n")

	b.WriteString("while IFS=$'\\t' read -r _action _blob_key _target; do\n")
	b.WriteString("  [ -n \"$_blob_key\" ] || continue\n")
	b.WriteString("  mkdir -p \"$(dirname \"$_target\")\"\n")
	b.WriteString("  \"$_action\" \"$_blob_stage/$_blob_key\" \"$_target\"\n")
	b.WriteString("done <<'EOF'\n")
	seenCount := make(map[string]int, len(src.Blobs))
	for _, blob := range src.Blobs {
		seenCount[blob.R2Key]++
		action := "cp"
		if seenCount[blob.R2Key] == keyCount[blob.R2Key] {
			action = "mv"
		}
		target := filepath.ToSlash(filepath.Join(src.RemoteDir, filepath.FromSlash(blob.RelPath)))
		b.WriteString(fmt.Sprintf("%s\t%s\t%s\n", action, blob.R2Key, target))
	}
	b.WriteString("EOF\n")

	b.WriteString("sha256sum -c - <<'EOF'\n")
	for _, blob := range src.Blobs {
		target := filepath.ToSlash(filepath.Join(src.RemoteDir, filepath.FromSlash(blob.RelPath)))
		b.WriteString(fmt.Sprintf("%s  %s\n", blob.SHA256, target))
	}
	b.WriteString("EOF\n")

	b.WriteString("rm -rf \"$_blob_stage\"\n")
	b.WriteString("trap - EXIT\n")
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
	b.WriteString("hf_validate_token\n")
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
	// Trailing `|| true` keeps the trap itself crash-proof if the helper
	// ever changes shape.
	b.WriteString(fmt.Sprintf(
		"trap '_weft_rc=$?; _weft_mark_stage %d \"failed:${_weft_rc}\" >/dev/null 2>&1 || true; exit ${_weft_rc}' ERR\n\n",
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

hf_validate_token() {
  # If HF_TOKEN is set, verify it's accepted by the Hub once up front.
  # huggingface-hub v1.x rewrites 401/403 into a misleading "not found"
  # error per model; pinging /whoami once gives an honest diagnostic and
  # lets us drop a bad token so anonymous downloads of public models can
  # still succeed.
  if [ -z "${HF_TOKEN:-}" ]; then
    return 0
  fi
  export HF_DEBUG=1
  local _ok=0
  if command -v hf >/dev/null 2>&1; then
    if hf auth whoami >/dev/null 2>&1; then _ok=1; fi
  elif command -v huggingface-cli >/dev/null 2>&1; then
    if huggingface-cli whoami >/dev/null 2>&1; then _ok=1; fi
  else
    return 0
  fi
  if [ "$_ok" -ne 1 ]; then
    echo 'weft: HF_TOKEN is set but rejected by the Hub — check expiry/scope. Unsetting HF_TOKEN and retrying anonymously.' >&2
    unset HF_TOKEN HUGGING_FACE_HUB_TOKEN HF_HUB_TOKEN
  fi
}

hf_download() {
  _repo_type="$1"
  _repo_id="$2"
  # Skip native-checkpoint dirs (e.g. Meta llama original/consolidated.*.pth)
  # for model repos; transformers/vLLM never load them.
  set -- --repo-type "$_repo_type"
  if [ "$_repo_type" = model ]; then set -- "$@" --exclude 'original/*'; fi
  if command -v hf >/dev/null 2>&1; then
    hf download "$@" "$_repo_id" 2>&1
  elif command -v huggingface-cli >/dev/null 2>&1; then
    huggingface-cli download "$@" "$_repo_id" 2>&1
  elif [ "$_repo_type" = model ]; then
    python3 -c 'import sys; from huggingface_hub import snapshot_download; snapshot_download(repo_id=sys.argv[1], repo_type=sys.argv[2], ignore_patterns=["original/*"])' "$_repo_id" "$_repo_type"
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
			skipFlags := dataloc.UVNoInstallPackageFlags(
				dataloc.ImageProvidedTorchPackages(filepath.Join(src.LocalDir, "uv.lock")))
			b.WriteString(fmt.Sprintf("cd %q && ([ -d .venv ] || ([ -n \"${UV_PYTHON:-}\" ] && \"$UV_PYTHON\" -m venv --system-site-packages .venv)) && uv sync%s 2>&1 || echo 'uv sync failed in %s'\n\n", src.RemoteDir, skipFlags, src.RemoteDir))
		} else {
			b.WriteString(fmt.Sprintf("cd %q && uv sync 2>&1 || echo 'uv sync failed in %s'\n\n", src.RemoteDir, src.RemoteDir))
		}
	}
	writeStageMarker(b, manifest.DBInstanceID, "deps_installed")

	writeHFDownloads(b, manifest.HFModels, manifest.HFDatasets, manifest.DBInstanceID)

	writeStageMarker(b, manifest.DBInstanceID, "ready")

	// Write ready marker to R2
	b.WriteString("# Signal readiness to Weft\n")
	b.WriteString(fmt.Sprintf(
		"echo 'ready' | rclone rcat \"r2:$R2_BUCKET/donor/%s/.ready\"\n",
		manifest.DonorID,
	))
	b.WriteString("echo 'Donor ready'\n")
}

// generateWorkerBootstrapTail generates the worker-specific portion:
// launches weft-agent run-instance via nohup.
func generateWorkerBootstrapTail(b *strings.Builder, manifest BootstrapManifest) {
	b.WriteString("# Launch instance agent\n")
	writeStageMarker(b, manifest.DBInstanceID, "agent_starting")
	if manifest.AgentForeground {
		b.WriteString("exec ")
	} else {
		b.WriteString("nohup ")
	}
	b.WriteString(fmt.Sprintf("weft-agent run-instance"+
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
