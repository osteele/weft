package campaign

import (
	"regexp"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/dataplane"
)

func TestBootstrapMaterializesSourceBlobsBeforeStartingAgent(t *testing.T) {
	script := GenerateBootstrapScript(BootstrapManifest{
		AgentR2Key: "agents/v2/linux-amd64",
		Sources: []SourceMapping{{
			R2Key: "sources/source.tar.gz", RemoteDir: "/workspace/project",
			Blobs: []dataplane.SourceBlob{{
				R2Key: "assets/abc123", RelPath: "data/training.pkl", SHA256: strings.Repeat("a", 64),
			}},
		}},
	})
	blobCopy := `rclone copyto "r2:$R2_BUCKET/assets/abc123" "/workspace/project/data/training.pkl"`
	if !strings.Contains(script, blobCopy) {
		t.Fatalf("bootstrap missing blob copy %q:\n%s", blobCopy, script)
	}
	if !strings.Contains(script, `sha256sum -c -`) {
		t.Fatalf("bootstrap missing blob SHA-256 verification:\n%s", script)
	}
	copyAt := strings.Index(script, blobCopy)
	startAt := strings.LastIndex(script, "weft-agent run-instance")
	if copyAt < 0 || startAt < 0 || copyAt > startAt {
		t.Fatalf("blob materialization must precede agent start (copy=%d start=%d)", copyAt, startAt)
	}
}

// TestStageMarkersAreNonFatal asserts every stage-marker write routes through
// _weft_mark_stage (retry + swallow) so a transient rcat failure cannot abort
// the bootstrap. Regression for EXP-021.
func TestStageMarkersAreNonFatal(t *testing.T) {
	script := GenerateBootstrapScript(BootstrapManifest{
		AgentR2Key:   "agent/linux-amd64",
		DBInstanceID: 12345,
		Sources: []SourceMapping{
			{R2Key: "sources/abc.tar.gz", RemoteDir: "/workspace/project"},
		},
	})

	if !strings.Contains(script, "_weft_mark_stage() {") {
		t.Fatalf("expected _weft_mark_stage helper definition in script:\n%s", script)
	}

	// All stage-marker writes must go through the helper.
	if !regexp.MustCompile(`_weft_mark_stage 12345 "agent_installing"`).MatchString(script) {
		t.Errorf("agent_installing marker not routed through _weft_mark_stage helper")
	}
	if !regexp.MustCompile(`_weft_mark_stage 12345 "agent_installed"`).MatchString(script) {
		t.Errorf("agent_installed marker not routed through _weft_mark_stage helper")
	}

	// No raw `echo … | rclone rcat …/bootstrap/…/stage` lines should remain.
	rawMarker := regexp.MustCompile(`echo\s+"[^"]*"\s*\|\s*rclone\s+rcat\s+"r2:[^"]*bootstrap/[0-9]+/stage"`)
	if loc := rawMarker.FindStringIndex(script); loc != nil {
		t.Errorf("found raw rcat stage-marker write (must use _weft_mark_stage):\n  %s",
			script[loc[0]:loc[1]])
	}

	// The helper itself must contain a retry loop and end with `return 0`
	// so a sustained R2 outage still allows the bootstrap to proceed.
	helperStart := strings.Index(script, "_weft_mark_stage() {")
	helperEnd := strings.Index(script[helperStart:], "\n}\n")
	if helperEnd < 0 {
		t.Fatalf("could not locate _weft_mark_stage helper body")
	}
	helper := script[helperStart : helperStart+helperEnd]
	if !strings.Contains(helper, "for _i in") {
		t.Errorf("_weft_mark_stage helper missing retry loop:\n%s", helper)
	}
	if !strings.Contains(helper, "return 0") {
		t.Errorf("_weft_mark_stage helper must swallow failure (return 0):\n%s", helper)
	}
}

// TestFailureTrapUsesStageHelper verifies the ERR trap routes its failure
// marker through the same retrying helper as ordinary stage writes.
func TestFailureTrapUsesStageHelper(t *testing.T) {
	script := GenerateBootstrapScript(BootstrapManifest{
		AgentR2Key:   "agent/linux-amd64",
		DBInstanceID: 99,
	})
	if !strings.Contains(script, `_weft_mark_stage 99 "failed:${_weft_rc}"`) {
		t.Errorf("failure trap does not route through _weft_mark_stage helper:\n%s", script)
	}
}
