package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
)

func TestDrainGraceJobRequestsAggregatesQueuedPayloads(t *testing.T) {
	instanceID := int64(55)
	prefix := controlplane.GraceJobsPrefix(instanceID)
	objects := map[string]string{}
	acks := map[string]controlplane.GraceCommandAck{}
	var deleted []string

	payloads := []controlplane.GraceJobsRequest{
		{Jobs: []cloud.AgentJob{{ID: 1, Command: "echo 1"}}},
		{Jobs: []cloud.AgentJob{{ID: 2, Command: "echo 2"}}},
	}
	for i, payload := range payloads {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("Marshal payload %d: %v", i, err)
		}
		requestID := []string{"req-a", "req-b"}[i]
		objects[path.Join(prefix, requestID+".json")] = string(data)
	}

	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() {
		graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete
	})

	graceR2List = func(_ string, gotPrefix string) ([]string, error) {
		if gotPrefix != prefix {
			t.Fatalf("prefix = %q, want %q", gotPrefix, prefix)
		}
		return []string{"req-a.json", "req-b.json"}, nil
	}
	graceR2Get = func(_ string, key string) (string, error) {
		return objects[key], nil
	}
	graceR2Put = func(_ string, key, content string) error {
		var ack controlplane.GraceCommandAck
		if err := json.Unmarshal([]byte(content), &ack); err != nil {
			t.Fatalf("Unmarshal ack: %v", err)
		}
		acks[key] = ack
		return nil
	}
	graceR2Delete = func(_ string, key string) error {
		deleted = append(deleted, key)
		return nil
	}

	jobs, err := drainGraceJobRequests("test-bucket", instanceID, nil)
	if err != nil {
		t.Fatalf("drainGraceJobRequests: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != 1 || jobs[1].ID != 2 {
		t.Fatalf("jobs = %+v, want IDs [1 2]", jobs)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted = %v, want both requests removed", deleted)
	}
	if len(acks) != 2 {
		t.Fatalf("acks = %v, want one ack per request", acks)
	}
	for _, ack := range acks {
		if !ack.Accepted || ack.Kind != controlplane.GraceCommandJobs {
			t.Fatalf("ack = %+v, want accepted jobs ack", ack)
		}
	}
}

func TestDrainGraceJobRequestsAppliesSourcesBeforeReturningJobs(t *testing.T) {
	instanceID := int64(56)
	prefix := controlplane.GraceJobsPrefix(instanceID)
	objects := map[string]string{}
	acks := map[string]controlplane.GraceCommandAck{}

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	payload := controlplane.GraceJobsRequest{
		Jobs: []cloud.AgentJob{{ID: 99, Command: "python train.py"}},
		Sources: []controlplane.SourceUpdate{{
			RemoteDir: remoteDir,
			R2Key:     "sources/test.tar.gz",
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	objects[path.Join(prefix, "req-a.json")] = string(data)

	prevCacheDir := sourceCacheDirOverride
	sourceCacheDirOverride = filepath.Join(t.TempDir(), "source-cache")
	t.Cleanup(func() { sourceCacheDirOverride = prevCacheDir })

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	rclonePath := filepath.Join(binDir, "rclone")
	rcloneScript := `#!/bin/sh
if [ "$1" != "copyto" ]; then
  echo "unexpected rclone command: $*" >&2
  exit 1
fi
dest="$3"
mkdir -p "$(dirname "$dest")"
printf 'stub' > "$dest"
`
	if err := os.WriteFile(rclonePath, []byte(rcloneScript), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	tarPath := filepath.Join(binDir, "tar")
	tarScript := `#!/bin/sh
if [ "$1" != "xzf" ] || [ "$3" != "-C" ]; then
  echo "unexpected tar command: $*" >&2
  exit 1
fi
dir="$4"
mkdir -p "$dir"
printf 'ok' > "$dir/source-applied.txt"
`
	if err := os.WriteFile(tarPath, []byte(tarScript), 0o755); err != nil {
		t.Fatalf("write fake tar: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() {
		graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete
	})

	graceR2List = func(_ string, gotPrefix string) ([]string, error) {
		if gotPrefix != prefix {
			t.Fatalf("prefix = %q, want %q", gotPrefix, prefix)
		}
		return []string{"req-a.json"}, nil
	}
	graceR2Get = func(_ string, key string) (string, error) {
		return objects[key], nil
	}
	graceR2Put = func(_ string, key, content string) error {
		var ack controlplane.GraceCommandAck
		if err := json.Unmarshal([]byte(content), &ack); err != nil {
			t.Fatalf("Unmarshal ack: %v", err)
		}
		acks[key] = ack
		return nil
	}
	graceR2Delete = func(_ string, _ string) error { return nil }

	var phases []string
	jobs, err := drainGraceJobRequests("test-bucket", instanceID, func(phase string) {
		phases = append(phases, phase)
	})
	if err != nil {
		t.Fatalf("drainGraceJobRequests: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != 99 {
		t.Fatalf("jobs = %+v, want one job id=99", jobs)
	}
	marker := filepath.Join(remoteDir, "source-applied.txt")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected source marker %s: %v", marker, err)
	}
	if len(acks) != 1 {
		t.Fatalf("acks = %v, want one", acks)
	}
	for _, ack := range acks {
		if !ack.Accepted {
			t.Fatalf("ack = %+v, want accepted", ack)
		}
	}
	for i, phase := range phases {
		if phase != "setup:99" {
			t.Fatalf("phase[%d] = %q, want setup:99 in %v", i, phase, phases)
		}
	}
	if len(phases) == 0 {
		t.Fatal("phases is empty, want setup attribution")
	}
}

func TestDrainGraceJobRequestsRejectsInvalidSourceUpdate(t *testing.T) {
	instanceID := int64(57)
	prefix := controlplane.GraceJobsPrefix(instanceID)
	objects := map[string]string{}
	acks := map[string]controlplane.GraceCommandAck{}

	payload := controlplane.GraceJobsRequest{
		Jobs: []cloud.AgentJob{{ID: 100, Command: "python train.py"}},
		Sources: []controlplane.SourceUpdate{{
			RemoteDir: "",
			R2Key:     "sources/test.tar.gz",
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	objects[path.Join(prefix, "req-a.json")] = string(data)

	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() {
		graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete
	})

	graceR2List = func(_ string, _ string) ([]string, error) { return []string{"req-a.json"}, nil }
	graceR2Get = func(_ string, key string) (string, error) { return objects[key], nil }
	graceR2Put = func(_ string, key, content string) error {
		var ack controlplane.GraceCommandAck
		if err := json.Unmarshal([]byte(content), &ack); err != nil {
			t.Fatalf("Unmarshal ack: %v", err)
		}
		acks[key] = ack
		return nil
	}
	graceR2Delete = func(_ string, _ string) error { return nil }

	jobs, err := drainGraceJobRequests("test-bucket", instanceID, nil)
	if err != nil {
		t.Fatalf("drainGraceJobRequests: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none", jobs)
	}
	if len(acks) != 1 {
		t.Fatalf("acks = %v, want one", acks)
	}
	for _, ack := range acks {
		if ack.Accepted {
			t.Fatalf("ack = %+v, want rejected", ack)
		}
	}
}

func TestApplySourceUpdateSameKeyDoesNotReextract(t *testing.T) {
	tarballDir := t.TempDir()
	writeSourceTarball(t, tarballDir, "same.tar.gz", map[string]string{
		"tracked.txt": "from tarball\n",
	})
	installFakeRcloneForSourceTarballs(t, tarballDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	upd := controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: "sources/same.tar.gz"}
	if err := applySourceUpdate("test-bucket", upd); err != nil {
		t.Fatalf("first applySourceUpdate: %v", err)
	}

	trackedPath := filepath.Join(remoteDir, "tracked.txt")
	if err := os.WriteFile(trackedPath, []byte("edited after extract\n"), 0o644); err != nil {
		t.Fatalf("edit extracted file: %v", err)
	}
	if err := applySourceUpdate("test-bucket", upd); err != nil {
		t.Fatalf("second applySourceUpdate: %v", err)
	}
	got, err := os.ReadFile(trackedPath)
	if err != nil {
		t.Fatalf("read tracked file: %v", err)
	}
	if string(got) != "edited after extract\n" {
		t.Fatalf("tracked.txt = %q, want edited content to survive same-key update", got)
	}
}

func TestApplySourceUpdateDifferentKeyCleansStaleFiles(t *testing.T) {
	tarballDir := t.TempDir()
	writeSourceTarball(t, tarballDir, "first.tar.gz", map[string]string{
		"current.txt": "first\n",
		"stale.txt":   "stale\n",
	})
	writeSourceTarball(t, tarballDir, "second.tar.gz", map[string]string{
		"current.txt": "second\n",
	})
	installFakeRcloneForSourceTarballs(t, tarballDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: "sources/first.tar.gz"}); err != nil {
		t.Fatalf("first applySourceUpdate: %v", err)
	}
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: "sources/second.tar.gz"}); err != nil {
		t.Fatalf("second applySourceUpdate: %v", err)
	}

	if _, err := os.Stat(filepath.Join(remoteDir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale.txt stat error = %v, want file absent after different-key update", err)
	}
	got, err := os.ReadFile(filepath.Join(remoteDir, "current.txt"))
	if err != nil {
		t.Fatalf("read current file: %v", err)
	}
	if string(got) != "second\n" {
		t.Fatalf("current.txt = %q, want second tarball content", got)
	}
}

func TestApplySourceUpdateRejectsDifferentKeyForRunningJobWorkdir(t *testing.T) {
	tarballDir := t.TempDir()
	writeSourceTarball(t, tarballDir, "first.tar.gz", map[string]string{
		"current.txt": "first\n",
	})
	writeSourceTarball(t, tarballDir, "second.tar.gz", map[string]string{
		"current.txt": "second\n",
	})
	installFakeRcloneForSourceTarballs(t, tarballDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: "sources/first.tar.gz"}); err != nil {
		t.Fatalf("first applySourceUpdate: %v", err)
	}
	doneUsingSource := activeSourceWorkdirs.begin(4242, remoteDir)
	defer doneUsingSource()

	err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: "sources/second.tar.gz"})
	if err == nil {
		t.Fatal("applySourceUpdate returned nil, want running-job workdir rejection")
	}
	if !strings.Contains(err.Error(), "running job 4242") {
		t.Fatalf("error = %q, want running job id", err)
	}
	got, readErr := os.ReadFile(filepath.Join(remoteDir, "current.txt"))
	if readErr != nil {
		t.Fatalf("read current file: %v", readErr)
	}
	if string(got) != "first\n" {
		t.Fatalf("current.txt = %q, want original tree to remain", got)
	}
}

func TestDrainGraceCancelAttemptRequestsAggregatesIDs(t *testing.T) {
	instanceID := int64(99)
	prefix := controlplane.GraceCancelAttemptsPrefix(instanceID)

	payloads := []controlplane.GraceCancelAttemptsRequest{
		{AttemptIDs: []int64{1001, 1002}},
		{AttemptIDs: []int64{1003}},
	}
	objects := map[string]string{}
	for i, payload := range payloads {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		objects[path.Join(prefix, []string{"req-a", "req-b"}[i]+".json")] = string(data)
	}

	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() {
		graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete
	})
	graceR2List = func(_ string, _ string) ([]string, error) {
		return []string{"req-a.json", "req-b.json"}, nil
	}
	graceR2Get = func(_ string, key string) (string, error) { return objects[key], nil }
	graceR2Put = func(_ string, _, _ string) error { return nil }
	graceR2Delete = func(_ string, _ string) error { return nil }

	got, err := drainGraceCancelAttemptRequests("test-bucket", instanceID)
	if err != nil {
		t.Fatalf("drainGraceCancelAttemptRequests: %v", err)
	}
	for _, want := range []int64{1001, 1002, 1003} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing canceled attempt id %d in %v", want, got)
		}
	}
}

func resetSourceUpdateState(t *testing.T) {
	t.Helper()
	prevCacheDir := sourceCacheDirOverride
	prevSources := sources
	prevActive := activeSourceWorkdirs
	sourceCacheDirOverride = filepath.Join(t.TempDir(), "source-cache")
	sources = &sourceRegistry{latest: map[string]registeredSource{}}
	activeSourceWorkdirs = &activeSourceWorkdirTracker{users: map[string]map[int64]int{}}
	t.Cleanup(func() {
		sourceCacheDirOverride = prevCacheDir
		sources = prevSources
		activeSourceWorkdirs = prevActive
	})
}

func writeSourceTarball(t *testing.T, tarballDir, name string, files map[string]string) {
	t.Helper()
	srcDir := filepath.Join(t.TempDir(), "src")
	for name, content := range files {
		filePath := filepath.Join(srcDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			t.Fatalf("mkdir source parent: %v", err)
		}
		if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
			t.Fatalf("write source file: %v", err)
		}
	}
	dest := filepath.Join(tarballDir, name)
	cmd := exec.Command("tar", "czf", dest, "-C", srcDir, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create source tarball %s: %v\n%s", name, err, output)
	}
}

func installFakeRcloneForSourceTarballs(t *testing.T, tarballDir string) {
	t.Helper()
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	rclonePath := filepath.Join(binDir, "rclone")
	rcloneScript := `#!/bin/sh
set -eu
if [ -n "${RCLONE_INVOCATION_LOG:-}" ]; then
  printf '%s\n' "$*" >> "$RCLONE_INVOCATION_LOG"
fi
source_for_key() {
  key="$1"
  source="$TARBALL_DIR/$key"
  if [ ! -f "$source" ]; then
    source="$TARBALL_DIR/${key##*/}"
  fi
  printf '%s\n' "$source"
}
copy_key() {
  key="$1"
  dest="$2"
  if [ "${RCLONE_FAIL_KEY:-}" = "$key" ]; then
    echo "forced failure for $key" >&2
    return 1
  fi
  source="$(source_for_key "$key")"
  mkdir -p "$(dirname "$dest")"
  cp "$source" "$dest"
}
case "$1" in
  copyto)
    src="$2"
    dest="$3"
    key="${src#*/}"
    copy_key "$key" "$dest"
    ;;
  copy)
    if [ "${RCLONE_FAIL_BATCH:-}" = "1" ]; then
      echo "forced batch failure" >&2
      exit 1
    fi
    dest="$3"
    if [ "$4" != "--files-from" ]; then
      echo "missing --files-from: $*" >&2
      exit 1
    fi
    while IFS= read -r key; do
      [ -n "$key" ] || continue
      copy_key "$key" "$dest/$key"
    done < "$5"
    ;;
  *)
    echo "unexpected rclone command: $*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(rclonePath, []byte(rcloneScript), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("TARBALL_DIR", tarballDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeFakeR2Object(t *testing.T, objectDir, key, content string) {
	t.Helper()
	filename := filepath.Join(objectDir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatalf("mkdir fake R2 object parent: %v", err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatalf("write fake R2 object: %v", err)
	}
}

func contentSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum)
}

func TestApplySourceUpdateMaterializesBlob(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "source.tar.gz", map[string]string{"main.py": "print('ok')\n"})
	blobContent := "large immutable input\n"
	blobHash := contentSHA256(blobContent)
	blobKey := "assets/" + blobHash
	writeFakeR2Object(t, objectDir, blobKey, blobContent)
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	upd := controlplane.SourceUpdate{
		RemoteDir: remoteDir,
		R2Key:     "sources/source.tar.gz",
		Blobs: []controlplane.SourceBlob{{
			R2Key:   blobKey,
			RelPath: "data/training.bin",
			SHA256:  blobHash,
		}},
	}
	if err := applySourceUpdate("test-bucket", upd); err != nil {
		t.Fatalf("applySourceUpdate: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(remoteDir, "data", "training.bin"))
	if err != nil {
		t.Fatalf("read materialized blob: %v", err)
	}
	if string(got) != blobContent {
		t.Fatalf("materialized blob = %q, want %q", got, blobContent)
	}
}

func TestApplySourceUpdateReusesCachedBlobAcrossUpdates(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "first.tar.gz", map[string]string{"version.txt": "first\n"})
	writeSourceTarball(t, objectDir, "second.tar.gz", map[string]string{"version.txt": "second\n"})
	blobContent := "shared blob\n"
	blobHash := contentSHA256(blobContent)
	blobKey := "assets/" + blobHash
	writeFakeR2Object(t, objectDir, blobKey, blobContent)
	invocationLog := filepath.Join(t.TempDir(), "rclone.log")
	t.Setenv("RCLONE_INVOCATION_LOG", invocationLog)
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	blobs := []controlplane.SourceBlob{{R2Key: blobKey, RelPath: "data/shared.bin", SHA256: blobHash}}
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: "sources/first.tar.gz", Blobs: blobs}); err != nil {
		t.Fatalf("applySourceUpdate first source: %v", err)
	}
	materializedPath := filepath.Join(remoteDir, "data", "shared.bin")
	if err := os.WriteFile(materializedPath, []byte("job mutated its input\n"), 0o644); err != nil {
		t.Fatalf("mutate materialized blob: %v", err)
	}
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: "sources/second.tar.gz", Blobs: blobs}); err != nil {
		t.Fatalf("applySourceUpdate second source: %v", err)
	}
	data, err := os.ReadFile(invocationLog)
	if err != nil {
		t.Fatalf("read rclone invocation log: %v", err)
	}
	batchCopies := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "copy ") {
			batchCopies++
		}
	}
	if batchCopies != 1 {
		t.Fatalf("blob batch download count = %d, want 1; invocations:\n%s", batchCopies, data)
	}
	got, err := os.ReadFile(materializedPath)
	if err != nil {
		t.Fatalf("read rematerialized blob: %v", err)
	}
	if string(got) != blobContent {
		t.Fatalf("rematerialized blob = %q, want cached content %q", got, blobContent)
	}
}

func TestApplySourceUpdateRefetchesCorruptedCachedBlob(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "first.tar.gz", map[string]string{"version.txt": "first\n"})
	writeSourceTarball(t, objectDir, "second.tar.gz", map[string]string{"version.txt": "second\n"})
	blobContent := "trusted blob\n"
	blobHash := contentSHA256(blobContent)
	blobKey := "assets/" + blobHash
	writeFakeR2Object(t, objectDir, blobKey, blobContent)
	invocationLog := filepath.Join(t.TempDir(), "rclone.log")
	t.Setenv("RCLONE_INVOCATION_LOG", invocationLog)
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	blobs := []controlplane.SourceBlob{{R2Key: blobKey, RelPath: "data/blob.bin", SHA256: blobHash}}
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: "sources/first.tar.gz", Blobs: blobs}); err != nil {
		t.Fatalf("first applySourceUpdate: %v", err)
	}
	if err := os.WriteFile(sourceBlobCachePath(blobHash), []byte("corrupted cache\n"), 0o644); err != nil {
		t.Fatalf("corrupt blob cache: %v", err)
	}
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: "sources/second.tar.gz", Blobs: blobs}); err != nil {
		t.Fatalf("second applySourceUpdate: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(remoteDir, "data", "blob.bin"))
	if err != nil {
		t.Fatalf("read rematerialized blob: %v", err)
	}
	if string(got) != blobContent {
		t.Fatalf("rematerialized blob = %q, want %q", got, blobContent)
	}
	data, err := os.ReadFile(invocationLog)
	if err != nil {
		t.Fatalf("read rclone invocation log: %v", err)
	}
	batchCopies := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "copy ") {
			batchCopies++
		}
	}
	if batchCopies != 2 {
		t.Fatalf("blob batch download count = %d, want 2 after cache corruption; invocations:\n%s", batchCopies, data)
	}
}

func TestDrainGraceJobRequestsRejectsBlobHashMismatch(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "source.tar.gz", map[string]string{"main.py": "print('ok')\n"})
	declaredHash := contentSHA256("expected bytes")
	blobKey := "assets/" + declaredHash
	writeFakeR2Object(t, objectDir, blobKey, "corrupted bytes")
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	instanceID := int64(58)
	prefix := controlplane.GraceJobsPrefix(instanceID)
	payload := controlplane.GraceJobsRequest{
		Jobs: []cloud.AgentJob{{ID: 101, Command: "python main.py"}},
		Sources: []controlplane.SourceUpdate{{
			RemoteDir: filepath.Join(t.TempDir(), "workspace", "project"),
			R2Key:     "sources/source.tar.gz",
			Blobs: []controlplane.SourceBlob{{
				R2Key: blobKey, RelPath: "data/input.bin", SHA256: declaredHash,
			}},
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	objects := map[string]string{path.Join(prefix, "req-a.json"): string(data)}
	var ack controlplane.GraceCommandAck
	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() { graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete })
	graceR2List = func(_ string, _ string) ([]string, error) { return []string{"req-a.json"}, nil }
	graceR2Get = func(_ string, key string) (string, error) { return objects[key], nil }
	graceR2Put = func(_ string, _ string, content string) error { return json.Unmarshal([]byte(content), &ack) }
	graceR2Delete = func(_ string, _ string) error { return nil }

	jobs, err := drainGraceJobRequests("test-bucket", instanceID, nil)
	if err != nil {
		t.Fatalf("drainGraceJobRequests: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want no runnable jobs after blob integrity failure", jobs)
	}
	if ack.Accepted || !strings.Contains(ack.Message, "has SHA-256") {
		t.Fatalf("ack = %+v, want rejected hash-mismatch failure", ack)
	}
}

func TestApplySourceUpdateBatchFailureNamesFailingBlob(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "source.tar.gz", map[string]string{"main.py": "print('ok')\n"})
	goodContent := "good\n"
	goodHash := contentSHA256(goodContent)
	goodKey := "assets/" + goodHash
	badHash := contentSHA256("missing\n")
	badKey := "assets/" + badHash
	writeFakeR2Object(t, objectDir, goodKey, goodContent)
	writeFakeR2Object(t, objectDir, badKey, "missing\n")
	t.Setenv("RCLONE_FAIL_BATCH", "1")
	t.Setenv("RCLONE_FAIL_KEY", badKey)
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{
		RemoteDir: filepath.Join(t.TempDir(), "workspace", "project"),
		R2Key:     "sources/source.tar.gz",
		Blobs: []controlplane.SourceBlob{
			{R2Key: goodKey, RelPath: "data/good.bin", SHA256: goodHash},
			{R2Key: badKey, RelPath: "data/failing.bin", SHA256: badHash},
		},
	})
	if err == nil {
		t.Fatal("applySourceUpdate returned nil, want blob download failure")
	}
	if !strings.Contains(err.Error(), badKey) || !strings.Contains(err.Error(), "data/failing.bin") {
		t.Fatalf("error = %q, want failing blob key and relative path", err)
	}
}

func TestApplySourceUpdateSameKeySkipsBlobWork(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "source.tar.gz", map[string]string{"tracked.txt": "original\n"})
	invocationLog := filepath.Join(t.TempDir(), "rclone.log")
	t.Setenv("RCLONE_INVOCATION_LOG", invocationLog)
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	sourceKey := "sources/source.tar.gz"
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: remoteDir, R2Key: sourceKey}); err != nil {
		t.Fatalf("first applySourceUpdate: %v", err)
	}
	before, err := os.ReadFile(invocationLog)
	if err != nil {
		t.Fatalf("read first invocation log: %v", err)
	}
	missingHash := contentSHA256("not present")
	err = applySourceUpdate("test-bucket", controlplane.SourceUpdate{
		RemoteDir: remoteDir,
		R2Key:     sourceKey,
		Blobs: []controlplane.SourceBlob{{
			R2Key: "assets/" + missingHash, RelPath: "data/missing.bin", SHA256: missingHash,
		}},
	})
	if err != nil {
		t.Fatalf("same-key applySourceUpdate performed blob work: %v", err)
	}
	after, err := os.ReadFile(invocationLog)
	if err != nil {
		t.Fatalf("read second invocation log: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("rclone invocations changed on same-key update:\nbefore: %s\nafter: %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(remoteDir, "data", "missing.bin")); !os.IsNotExist(err) {
		t.Fatalf("missing blob stat error = %v, want no blob work", err)
	}
}

func TestApplySourceUpdateBlobFailurePreservesExistingTree(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "replacement.tar.gz", map[string]string{"replacement.txt": "new tree\n"})
	missingHash := contentSHA256("missing blob")
	missingKey := "assets/" + missingHash
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("mkdir existing tree: %v", err)
	}
	existingPath := filepath.Join(remoteDir, "existing.txt")
	if err := os.WriteFile(existingPath, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("write existing tree: %v", err)
	}

	err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{
		RemoteDir: remoteDir,
		R2Key:     "sources/replacement.tar.gz",
		Blobs: []controlplane.SourceBlob{{
			R2Key: missingKey, RelPath: "data/missing.bin", SHA256: missingHash,
		}},
	})
	if err == nil {
		t.Fatal("applySourceUpdate returned nil, want missing blob failure")
	}
	got, readErr := os.ReadFile(existingPath)
	if readErr != nil {
		t.Fatalf("existing tree was destroyed after blob failure: %v", readErr)
	}
	if string(got) != "keep me\n" {
		t.Fatalf("existing tree content = %q, want preserved content", got)
	}
	if _, statErr := os.Stat(filepath.Join(remoteDir, "replacement.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("replacement tree stat error = %v, want replacement not extracted", statErr)
	}
}

func TestApplySourceUpdateRejectsUpdateToSiblingMountOfRunningJob(t *testing.T) {
	tarballDir := t.TempDir()
	writeSourceTarball(t, tarballDir, "lib-first.tar.gz", map[string]string{
		"lib.py": "first\n",
	})
	writeSourceTarball(t, tarballDir, "lib-second.tar.gz", map[string]string{
		"lib.py": "second\n",
	})
	installFakeRcloneForSourceTarballs(t, tarballDir)
	resetSourceUpdateState(t)

	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	siblingDir := filepath.Join(root, "shared-lib")
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: siblingDir, R2Key: "sources/lib-first.tar.gz"}); err != nil {
		t.Fatalf("first applySourceUpdate: %v", err)
	}

	// The running job's working directory is projectDir; siblingDir is a uv
	// path dep it imports from. Both must be held.
	job := cloud.AgentJob{
		ID:  7,
		Dir: projectDir,
		SourceMounts: []cloud.SourceMount{
			{RemoteDir: projectDir},
			{RemoteDir: siblingDir},
		},
	}
	doneUsingSource := activeSourceWorkdirs.beginMounts(job.ID, expandedSourceMountsForJob(job))

	err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: siblingDir, R2Key: "sources/lib-second.tar.gz"})
	if err == nil {
		t.Fatal("applySourceUpdate returned nil, want rejection for sibling mount of running job")
	}
	if !strings.Contains(err.Error(), "running job 7") {
		t.Fatalf("error = %q, want running job id", err)
	}
	got, readErr := os.ReadFile(filepath.Join(siblingDir, "lib.py"))
	if readErr != nil {
		t.Fatalf("read sibling file: %v", readErr)
	}
	if string(got) != "first\n" {
		t.Fatalf("lib.py = %q, want sibling tree to remain", got)
	}

	// After the job releases, the same update must succeed.
	doneUsingSource()
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{RemoteDir: siblingDir, R2Key: "sources/lib-second.tar.gz"}); err != nil {
		t.Fatalf("applySourceUpdate after release: %v", err)
	}
	got, readErr = os.ReadFile(filepath.Join(siblingDir, "lib.py"))
	if readErr != nil {
		t.Fatalf("read sibling file after release: %v", readErr)
	}
	if string(got) != "second\n" {
		t.Fatalf("lib.py = %q, want updated tree after release", got)
	}
}
