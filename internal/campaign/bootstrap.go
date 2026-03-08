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
}

// SourceMapping maps an R2 key to a target directory on the instance.
type SourceMapping struct {
	R2Key     string // e.g. "sources/sha256abc.tar.gz"
	RemoteDir string // e.g. "/workspace/my-project"
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
	b.WriteString("chmod +x /usr/local/bin/weft-agent\n\n")

	// Download and extract source tarballs
	for _, src := range manifest.Sources {
		b.WriteString(fmt.Sprintf("# Extract source to %s\n", src.RemoteDir))
		b.WriteString(fmt.Sprintf("mkdir -p %q\n", src.RemoteDir))
		b.WriteString(fmt.Sprintf(
			"rclone copyto \"r2:$R2_BUCKET/%s\" /tmp/src.tar.gz && tar xzf /tmp/src.tar.gz -C %q && rm -f /tmp/src.tar.gz\n\n",
			src.R2Key, src.RemoteDir,
		))
	}

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

	return b.String()
}
