package campaign

import (
	"fmt"
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
	batchCopy := `rclone copy "r2:$R2_BUCKET" "$_blob_stage" --files-from "$_blob_stage/files-from.txt"`
	if !strings.Contains(script, batchCopy) {
		t.Fatalf("bootstrap missing batch blob copy %q:\n%s", batchCopy, script)
	}
	if !strings.Contains(script, `_blob_stage=$(mktemp -d "/workspace/project/.weft-blobs.XXXXXX")`) {
		t.Fatalf("bootstrap missing staging directory under remote dir:\n%s", script)
	}
	if !strings.Contains(script, `trap 'rm -rf "${_blob_stage:-}"' EXIT`) {
		t.Fatalf("bootstrap missing EXIT trap for staging cleanup:\n%s", script)
	}
	if !strings.Contains(script, "trap - EXIT") {
		t.Fatalf("bootstrap missing EXIT trap clearing:\n%s", script)
	}
	if !strings.Contains(script, "assets/abc123") {
		t.Fatalf("bootstrap missing blob key:\n%s", script)
	}
	if !strings.Contains(script, "\"$_action\" \"$_blob_stage/$_blob_key\" \"$_target\"") {
		t.Fatalf("bootstrap missing blob placement loop:\n%s", script)
	}
	if !strings.Contains(script, "mv\tassets/abc123\t/workspace/project/data/training.pkl") {
		t.Fatalf("bootstrap missing mv target path:\n%s", script)
	}
	if !strings.Contains(script, `sha256sum -c -`) {
		t.Fatalf("bootstrap missing blob SHA-256 verification:\n%s", script)
	}
	// Fallback per-blob download must be present
	if !strings.Contains(script, `rclone copyto "r2:$R2_BUCKET/$_blob_key" "$_blob_stage/$_blob_key"`) {
		t.Fatalf("bootstrap missing fallback blob download:\n%s", script)
	}
	copyAt := strings.Index(script, batchCopy)
	startAt := strings.LastIndex(script, "weft-agent run-instance")
	if copyAt < 0 || startAt < 0 || copyAt > startAt {
		t.Fatalf("blob materialization must precede agent start (copy=%d start=%d)", copyAt, startAt)
	}
}

func TestBootstrapBatchesMultipleSourceBlobsAndDeduplicatesKeys(t *testing.T) {
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	script := GenerateBootstrapScript(BootstrapManifest{
		AgentR2Key: "agents/v2/linux-amd64",
		Sources: []SourceMapping{{
			R2Key: "sources/source.tar.gz", RemoteDir: "/workspace/project",
			Blobs: []dataplane.SourceBlob{
				{R2Key: "assets/blob1", RelPath: "data/file1.bin", SHA256: hashA},
				{R2Key: "assets/blob1", RelPath: "data/file1_copy.bin", SHA256: hashA},
				{R2Key: "assets/blob2", RelPath: "data/file2.bin", SHA256: hashB},
			},
		}},
	})

	// Check files-from list contains unique keys
	filesFromSection := script[strings.Index(script, "files-from.txt"):strings.Index(script, "if ! rclone copy")]
	if strings.Count(filesFromSection, "assets/blob1") != 1 {
		t.Fatalf("files-from.txt should deduplicate blob keys, got:\n%s", filesFromSection)
	}
	if strings.Count(filesFromSection, "assets/blob2") != 1 {
		t.Fatalf("files-from.txt missing blob2, got:\n%s", filesFromSection)
	}

	// Check placement maps all 3 targets: earlier duplicate uses cp, final uses mv
	if !strings.Contains(script, "cp\tassets/blob1\t/workspace/project/data/file1.bin") {
		t.Fatalf("missing target file1.bin (cp) in script:\n%s", script)
	}
	if !strings.Contains(script, "mv\tassets/blob1\t/workspace/project/data/file1_copy.bin") {
		t.Fatalf("missing target file1_copy.bin (mv) in script:\n%s", script)
	}
	if !strings.Contains(script, "mv\tassets/blob2\t/workspace/project/data/file2.bin") {
		t.Fatalf("missing target file2.bin (mv) in script:\n%s", script)
	}

	// Check sha256sum verifies all 3 targets
	if !strings.Contains(script, fmt.Sprintf("%s  /workspace/project/data/file1.bin", hashA)) {
		t.Fatalf("missing checksum for file1.bin:\n%s", script)
	}
	if !strings.Contains(script, fmt.Sprintf("%s  /workspace/project/data/file1_copy.bin", hashA)) {
		t.Fatalf("missing checksum for file1_copy.bin:\n%s", script)
	}
	if !strings.Contains(script, fmt.Sprintf("%s  /workspace/project/data/file2.bin", hashB)) {
		t.Fatalf("missing checksum for file2.bin:\n%s", script)
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
