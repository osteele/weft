package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ssh"
)

type fakeDataPublishR2 struct {
	objects map[string][]byte
}

func (f *fakeDataPublishR2) ObjectExists(_ context.Context, key string) (bool, error) {
	_, ok := f.objects[key]
	return ok, nil
}

func (f *fakeDataPublishR2) PutObject(_ context.Context, key string, body io.Reader, _ string) error {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, body); err != nil {
		return err
	}
	f.objects[key] = buf.Bytes()
	return nil
}

func setupDataPublishTest(t *testing.T, r2Client dataPublishR2Client) {
	t.Helper()
	db.SetupTestDB(t)
	oldName := dataPublishName
	oldTargetPath := dataPublishTargetPath
	oldHost := dataPublishHost
	oldBuildR2 := dataPublishBuildR2Client
	oldCopyFrom := dataPublishCopyFrom
	oldDigestFromHost := dataPublishDigestFromHost
	oldPutFromHost := dataPublishPutFromHost
	t.Cleanup(func() {
		dataPublishName = oldName
		dataPublishTargetPath = oldTargetPath
		dataPublishHost = oldHost
		dataPublishBuildR2Client = oldBuildR2
		dataPublishCopyFrom = oldCopyFrom
		dataPublishDigestFromHost = oldDigestFromHost
		dataPublishPutFromHost = oldPutFromHost
	})
	dataPublishName = ""
	dataPublishTargetPath = ""
	dataPublishHost = ""
	dataPublishBuildR2Client = func(_ *config.Config) (dataPublishR2Client, error) {
		return r2Client, nil
	}
}

func TestRunDataFetchRecordsCompletedRequest(t *testing.T) {
	database := db.SetupTestDB(t)

	cleanupHosts := inventory.SetHosts([]inventory.HostSpec{{
		Name:     "cool30",
		OS:       "linux",
		Arch:     "amd64",
		CPUCores: 16,
		Memory:   "64 GiB",
		GPUs: []inventory.GPUSpec{{
			Name:    "RTX 3090",
			Class:   "rtx3090",
			Memory:  "24 GiB",
			Indices: []int{0},
		}},
	}})
	t.Cleanup(cleanupHosts)

	cleanupSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		if host != "cool30" {
			return "", "", fmt.Errorf("unexpected host %q", host)
		}
		switch {
		case strings.Contains(command, "df -Pk"):
			return "20971520\n", "", nil
		// Daemonized download path (see runDetachedRemoteCommand in
		// internal/dataloc/download.go): the spawn issues a `nohup ...
		// cmd.sh ...` invocation; the poll calls `if [ -f "$D/status"
		// ]`. We don't actually need to run anything here — the test only
		// cares that the request transitions to "completed" — so the spawn
		// returns "OK" and the first poll claims STATUS=0 with no stderr.
		case strings.Contains(command, "nohup") && strings.Contains(command, "cmd.sh"):
			return "OK\n", "", nil
		case strings.Contains(command, `if [ -f "$D/status" ]`):
			return "STATUS=0\n---STDERR---\n", "", nil
		case strings.Contains(command, "$_hfdl --repo-type model"):
			return "", "", nil
		case strings.Contains(command, "du -sb"), strings.Contains(command, "ls -1d"):
			return "2048\tok\t/home/test/.cache/huggingface/hub/models--bert-base-uncased\n", "", nil
		default:
			return "", "", fmt.Errorf("unexpected command %q", command)
		}
	})
	t.Cleanup(cleanupSSH)
	// Tighten poll interval so the test doesn't sit on the default 5s wait.
	cleanupPoll := dataloc.SetDetachedPollIntervalForTest(time.Millisecond)
	t.Cleanup(cleanupPoll)

	dataFetchHost = "cool30"
	dataFetchRev = "main"
	dataJSON = false

	out := captureStdout(t, func() {
		if err := runDataFetch(nil, []string{"hf:bert-base-uncased"}); err != nil {
			t.Fatalf("runDataFetch: %v", err)
		}
	})
	if !strings.Contains(out, "Request 1 completed") {
		t.Fatalf("output = %q", out)
	}

	requests, err := dataloc.ListDataRequests(database, "cool30")
	if err != nil {
		t.Fatalf("ListDataRequests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(requests))
	}
	if requests[0].Status != dataloc.RequestCompleted {
		t.Fatalf("status = %s, want %s", requests[0].Status, dataloc.RequestCompleted)
	}

	entries, err := dataloc.FindAssetHosts(database, dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "bert-base-uncased"})
	if err != nil {
		t.Fatalf("FindAssetHosts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].SizeBytes != 2048 {
		t.Fatalf("size = %d, want 2048", entries[0].SizeBytes)
	}
}

func TestRunDataWherePrintsKnownHosts(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host:      "cool100",
		Asset:     dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "meta-llama/Llama-3-8B"},
		Path:      "/cache/models--meta-llama--Llama-3-8B",
		SizeBytes: 4096,
		LastSeen:  now,
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	dataJSON = false
	out := captureStdout(t, func() {
		if err := runDataWhere(nil, []string{"hf:meta-llama/Llama-3-8B"}); err != nil {
			t.Fatalf("runDataWhere: %v", err)
		}
	})
	if !strings.Contains(out, "cool100") {
		t.Fatalf("output missing host: %q", out)
	}
	if !strings.Contains(out, "HOST") || !strings.Contains(out, "PATH") {
		t.Fatalf("output missing table headers: %q", out)
	}
}

func TestParseDataAssetArgAcceptsCheckpoint(t *testing.T) {
	asset, err := parseDataAssetArg("checkpoint:model")
	if err != nil {
		t.Fatalf("expected checkpoint asset to be accepted, got: %v", err)
	}
	if asset.Kind != dataloc.AssetCheckpoint {
		t.Fatalf("kind = %s, want %s", asset.Kind, dataloc.AssetCheckpoint)
	}
	if asset.ID != "model" {
		t.Fatalf("id = %s, want model", asset.ID)
	}
}

func TestRunDataAddRegistersCheckpoint(t *testing.T) {
	database := db.SetupTestDB(t)
	path := filepath.Join(t.TempDir(), "checkpoint.bin")
	if err := os.WriteFile(path, []byte("checkpoint bytes"), 0o644); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}

	dataAddHost = "studio"
	dataAddName = "test-checkpoint"
	t.Cleanup(func() {
		dataAddHost = ""
		dataAddName = ""
	})

	out := captureStdout(t, func() {
		if err := runDataAdd(nil, []string{path}); err != nil {
			t.Fatalf("runDataAdd: %v", err)
		}
	})
	if !strings.Contains(out, "checkpoint:test-checkpoint") {
		t.Fatalf("output missing asset ref: %q", out)
	}
	if !strings.Contains(out, "studio") {
		t.Fatalf("output missing host: %q", out)
	}

	entries, err := dataloc.FindAssetHosts(database, dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "test-checkpoint"})
	if err != nil {
		t.Fatalf("FindAssetHosts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Host != "studio" {
		t.Fatalf("host = %s, want studio", entries[0].Host)
	}
	if entries[0].Path != path {
		t.Fatalf("path = %s, want %s", entries[0].Path, path)
	}
	if entries[0].SizeBytes != int64(len("checkpoint bytes")) {
		t.Fatalf("size = %d", entries[0].SizeBytes)
	}
	if entries[0].ContentHash == "" {
		t.Fatalf("content hash was not recorded")
	}
	if entries[0].ContentType != dataloc.ContentTypeFile {
		t.Fatalf("content type = %q, want file", entries[0].ContentType)
	}
}

func TestRunDataAddRegistersDirectoryCheckpointDigestAndSize(t *testing.T) {
	database := db.SetupTestDB(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte("bbb"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "a.bin"), []byte("aaaa"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}

	dataAddHost = "studio"
	dataAddName = "dir-checkpoint"
	t.Cleanup(func() {
		dataAddHost = ""
		dataAddName = ""
	})

	if err := runDataAdd(nil, []string{dir}); err != nil {
		t.Fatalf("runDataAdd: %v", err)
	}

	entries, err := dataloc.FindAssetHosts(database, dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "dir-checkpoint"})
	if err != nil {
		t.Fatalf("FindAssetHosts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].SizeBytes != 7 {
		t.Fatalf("size = %d, want 7", entries[0].SizeBytes)
	}
	if entries[0].ContentHash == "" {
		t.Fatalf("content hash was not recorded")
	}
	if entries[0].ContentType != dataloc.ContentTypeDirectory {
		t.Fatalf("content type = %q, want directory", entries[0].ContentType)
	}
}

func TestDeriveCheckpointNameBasename(t *testing.T) {
	name := deriveCheckpointName("/tmp/some-random-dir/my-checkpoint")
	if name != "my-checkpoint" {
		t.Fatalf("name = %s, want my-checkpoint", name)
	}
}

func TestDerivePublishTargetPathFromRelativeRepoPath(t *testing.T) {
	got := derivePublishTargetPath(filepath.Join("..", "testdata", "pytorch-training", "train.py"))
	want := "testdata/pytorch-training/train.py"
	if got != want {
		t.Fatalf("target path = %q, want %q", got, want)
	}
}

func TestRunDataPublishDefaultsToRelativeRepoPath(t *testing.T) {
	r2Client := &fakeDataPublishR2{objects: make(map[string][]byte)}
	setupDataPublishTest(t, r2Client)
	dataPublishName = "training-script"

	if err := runDataPublish(nil, []string{filepath.Join("..", "testdata", "pytorch-training", "train.py")}); err != nil {
		t.Fatalf("runDataPublish: %v", err)
	}

	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	asset, err := db.GetNamedAssetByName(database, "training-script")
	if err != nil {
		t.Fatalf("GetNamedAssetByName: %v", err)
	}
	if asset.TargetPath != "testdata/pytorch-training/train.py" {
		t.Fatalf("target path = %q, want repository-relative path", asset.TargetPath)
	}
}

func TestRunDataWhereCheckpoint(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host:     "studio",
		Asset:    dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "my-ckpt"},
		Path:     "~/code/research/LM2/runs/my-ckpt",
		LastSeen: now,
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	dataJSON = false
	out := captureStdout(t, func() {
		if err := runDataWhere(nil, []string{"checkpoint:my-ckpt"}); err != nil {
			t.Fatalf("runDataWhere: %v", err)
		}
	})
	if !strings.Contains(out, "studio") {
		t.Fatalf("output missing host: %q", out)
	}
}

func TestRunDataPublishRemoteHostFlag(t *testing.T) {
	r2Client := &fakeDataPublishR2{objects: make(map[string][]byte)}
	setupDataPublishTest(t, r2Client)

	dataPublishName = "remote-trace"
	dataPublishHost = "cool30"
	dataPublishTargetPath = "data/mooncake/toolagent_trace.jsonl"
	content := dataloc.ContentInfo{
		Hash:        strings.Repeat("a", 64),
		SizeBytes:   int64(len("trace data")),
		ContentType: dataloc.ContentTypeFile,
	}
	dataPublishDigestFromHost = func(_ context.Context, host, remotePath string) (dataloc.ContentInfo, error) {
		if host != "cool30" {
			return dataloc.ContentInfo{}, fmt.Errorf("host = %s, want cool30", host)
		}
		if remotePath != "/remote/toolagent_trace.jsonl" {
			return dataloc.ContentInfo{}, fmt.Errorf("remotePath = %s", remotePath)
		}
		return content, nil
	}
	putCalls := 0
	dataPublishPutFromHost = func(_ context.Context, host string, req dataloc.R2PutFromHostRequest) error {
		putCalls++
		if host != "cool30" || req.Path != "/remote/toolagent_trace.jsonl" {
			return fmt.Errorf("put = %s:%s", host, req.Path)
		}
		if req.Key != "assets/"+content.Hash {
			return fmt.Errorf("key = %s", req.Key)
		}
		r2Client.objects[req.Key] = []byte("uploaded remotely")
		return nil
	}
	dataPublishCopyFrom = func(_, _, _ string) error {
		return fmt.Errorf("copy-from-host path must not be used")
	}

	out := captureStdout(t, func() {
		if err := runDataPublish(nil, []string{"/remote/toolagent_trace.jsonl"}); err != nil {
			t.Fatalf("runDataPublish: %v", err)
		}
	})
	if !strings.Contains(out, "Published asset:remote-trace") {
		t.Fatalf("output missing publish line: %q", out)
	}
	if putCalls != 1 {
		t.Fatalf("putCalls = %d, want 1", putCalls)
	}

	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	asset, err := db.GetNamedAssetByName(database, "remote-trace")
	if err != nil {
		t.Fatalf("GetNamedAssetByName: %v", err)
	}
	if asset.TargetPath != "data/mooncake/toolagent_trace.jsonl" {
		t.Fatalf("target path = %s", asset.TargetPath)
	}
	if asset.SizeBytes != int64(len("trace data")) {
		t.Fatalf("size = %d", asset.SizeBytes)
	}
}

func TestRunDataPublishRemoteSCPStyle(t *testing.T) {
	r2Client := &fakeDataPublishR2{objects: make(map[string][]byte)}
	setupDataPublishTest(t, r2Client)

	dataPublishName = "remote-trace"
	dataPublishTargetPath = "data/mooncake/toolagent_trace.jsonl"
	dataPublishDigestFromHost = func(_ context.Context, host, remotePath string) (dataloc.ContentInfo, error) {
		if host != "cool30" || remotePath != "/remote/toolagent_trace.jsonl" {
			return dataloc.ContentInfo{}, fmt.Errorf("digest = %s:%s", host, remotePath)
		}
		return dataloc.ContentInfo{Hash: strings.Repeat("b", 64), SizeBytes: int64(len("trace data")), ContentType: dataloc.ContentTypeFile}, nil
	}
	putCalls := 0
	dataPublishPutFromHost = func(_ context.Context, host string, req dataloc.R2PutFromHostRequest) error {
		putCalls++
		if host != "cool30" || req.Path != "/remote/toolagent_trace.jsonl" {
			return fmt.Errorf("put = %s:%s", host, req.Path)
		}
		r2Client.objects[req.Key] = []byte("uploaded remotely")
		return nil
	}
	dataPublishCopyFrom = func(_, _, _ string) error {
		return fmt.Errorf("copy-from-host path must not be used")
	}

	if err := runDataPublish(nil, []string{"cool30:/remote/toolagent_trace.jsonl"}); err != nil {
		t.Fatalf("runDataPublish: %v", err)
	}
	if putCalls != 1 {
		t.Fatalf("putCalls = %d, want 1", putCalls)
	}
}

func TestRunDataPublishRemoteSkipsUploadWhenContentAlreadyInR2(t *testing.T) {
	hash := strings.Repeat("e", 64)
	r2Client := &fakeDataPublishR2{objects: map[string][]byte{"assets/" + hash: []byte("already there")}}
	setupDataPublishTest(t, r2Client)

	dataPublishName = "remote-trace"
	dataPublishTargetPath = "data/mooncake/toolagent_trace.jsonl"
	dataPublishDigestFromHost = func(_ context.Context, host, remotePath string) (dataloc.ContentInfo, error) {
		if host != "cool30" || remotePath != "/remote/toolagent_trace.jsonl" {
			return dataloc.ContentInfo{}, fmt.Errorf("digest = %s:%s", host, remotePath)
		}
		return dataloc.ContentInfo{Hash: hash, SizeBytes: int64(len("trace data")), ContentType: dataloc.ContentTypeFile}, nil
	}
	dataPublishPutFromHost = func(context.Context, string, dataloc.R2PutFromHostRequest) error {
		t.Fatal("remote put should not run when object already exists")
		return nil
	}
	dataPublishCopyFrom = func(_, _, _ string) error {
		return fmt.Errorf("copy-from-host path must not be used")
	}

	if err := runDataPublish(nil, []string{"cool30:/remote/toolagent_trace.jsonl"}); err != nil {
		t.Fatalf("runDataPublish: %v", err)
	}
	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	asset, err := db.GetNamedAssetByName(database, "remote-trace")
	if err != nil {
		t.Fatalf("GetNamedAssetByName: %v", err)
	}
	if asset.ContentHash != hash {
		t.Fatalf("hash = %s, want %s", asset.ContentHash, hash)
	}
}

func TestRunDataPublishDirectoryUploadsArchiveAndRecordsDirectoryAsset(t *testing.T) {
	r2Client := &fakeDataPublishR2{objects: make(map[string][]byte)}
	setupDataPublishTest(t, r2Client)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), []byte("one"), 0o644); err != nil {
		t.Fatalf("write one: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two.txt"), []byte("two2"), 0o644); err != nil {
		t.Fatalf("write two: %v", err)
	}
	dataPublishName = "dir-asset"
	dataPublishTargetPath = "data/checkpoint"

	if err := runDataPublish(nil, []string{dir}); err != nil {
		t.Fatalf("runDataPublish: %v", err)
	}
	if len(r2Client.objects) != 1 {
		t.Fatalf("uploaded objects = %d, want 1", len(r2Client.objects))
	}

	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	asset, err := db.GetNamedAssetByName(database, "dir-asset")
	if err != nil {
		t.Fatalf("GetNamedAssetByName: %v", err)
	}
	if asset.SizeBytes != 7 {
		t.Fatalf("size = %d, want 7", asset.SizeBytes)
	}
	if asset.ContentType != string(dataloc.ContentTypeDirectory) {
		t.Fatalf("content type = %q, want directory", asset.ContentType)
	}
}

func TestRunDataPublishCheckpointPromotesRegisteredDirectory(t *testing.T) {
	r2Client := &fakeDataPublishR2{objects: make(map[string][]byte)}
	setupDataPublishTest(t, r2Client)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "weights.bin"), []byte("weights"), 0o644); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	content, err := dataloc.DigestPath(dir)
	if err != nil {
		t.Fatalf("DigestPath: %v", err)
	}
	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host:        "studio",
		Asset:       dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "ckpt-v1"},
		Path:        dir,
		SizeBytes:   content.SizeBytes,
		ContentHash: content.Hash,
		ContentType: content.ContentType,
		LastSeen:    time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	dataPublishName = "ckpt-v1"
	dataPublishTargetPath = "checkpoints/ckpt-v1"
	if err := runDataPublish(nil, []string{"checkpoint:ckpt-v1"}); err != nil {
		t.Fatalf("runDataPublish: %v", err)
	}

	asset, err := db.GetNamedAssetByName(database, "ckpt-v1")
	if err != nil {
		t.Fatalf("GetNamedAssetByName: %v", err)
	}
	if asset.ContentHash != content.Hash {
		t.Fatalf("hash = %s, want %s", asset.ContentHash, content.Hash)
	}
	if asset.SizeBytes != content.SizeBytes {
		t.Fatalf("size = %d, want %d", asset.SizeBytes, content.SizeBytes)
	}
	if asset.ContentType != string(dataloc.ContentTypeDirectory) {
		t.Fatalf("content type = %q, want directory", asset.ContentType)
	}
}

func TestRunDataPublishRemoteRejectsAmbiguousHostSyntax(t *testing.T) {
	setupDataPublishTest(t, &fakeDataPublishR2{objects: make(map[string][]byte)})
	dataPublishName = "remote-trace"
	dataPublishHost = "cool30"
	dataPublishTargetPath = "data/file"

	err := runDataPublish(nil, []string{"cool100:/remote/file"})
	if err == nil || !strings.Contains(err.Error(), "cannot combine --host with host:path source") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunDataPublishRemoteRequiresTargetPath(t *testing.T) {
	setupDataPublishTest(t, &fakeDataPublishR2{objects: make(map[string][]byte)})
	dataPublishName = "remote-trace"
	dataPublishHost = "cool30"
	dataPublishDigestFromHost = func(context.Context, string, string) (dataloc.ContentInfo, error) {
		return dataloc.ContentInfo{Hash: strings.Repeat("c", 64), SizeBytes: int64(len("trace data")), ContentType: dataloc.ContentTypeFile}, nil
	}

	err := runDataPublish(nil, []string{"/remote/file"})
	if err == nil || !strings.Contains(err.Error(), "remote publish requires --target-path") {
		t.Fatalf("err = %v", err)
	}
}

func TestIsValidDataFetchHostAllowsLocalhost(t *testing.T) {
	if !isValidDataFetchHost("localhost") {
		t.Fatal("localhost should be accepted as a special-case fetch target")
	}
}
