package main

import (
	"encoding/json"
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
	sources = &sourceRegistry{latest: map[string]string{}}
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
if [ "$1" != "copyto" ]; then
  echo "unexpected rclone command: $*" >&2
  exit 1
fi
src="$2"
dest="$3"
base="${src##*/}"
mkdir -p "$(dirname "$dest")"
cp "$TARBALL_DIR/$base" "$dest"
`
	if err := os.WriteFile(rclonePath, []byte(rcloneScript), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("TARBALL_DIR", tarballDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
