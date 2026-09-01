package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

type memoryPayloadStore struct {
	objects map[string][]byte
}

func (s *memoryPayloadStore) ObjectExists(_ context.Context, key string) (bool, error) {
	_, ok := s.objects[key]
	return ok, nil
}

func (s *memoryPayloadStore) PutObject(_ context.Context, key string, body io.Reader, _ string) error {
	data, err := io.ReadAll(body)
	if err == nil {
		s.objects[key] = data
	}
	return err
}

func TestParseRunPayloadDeclarations(t *testing.T) {
	got, err := parseRunPayloadDeclarations([]string{"config=/tmp/a", "token=relative.bin"})
	if err != nil || len(got) != 2 || got[0].Name != "config" || got[1].Path != "relative.bin" {
		t.Fatalf("parse = %+v, %v", got, err)
	}
	for _, values := range [][]string{{"missing-equals"}, {"bad/name=x"}, {"dup=a", "dup=b"}, {"empty="}} {
		if _, err := parseRunPayloadDeclarations(values); err == nil {
			t.Errorf("parse %v succeeded", values)
		}
	}
}

func TestCapturedPayloadSurvivesSourceDeletionAndRetry(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := &memoryPayloadStore{objects: make(map[string][]byte)}
	previous := buildRunPayloadObjectStore
	buildRunPayloadObjectStore = func() (runPayloadObjectStore, error) { return store, nil }
	t.Cleanup(func() { buildRunPayloadObjectStore = previous })

	source := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(source, []byte("stable bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	payloads, err := captureRunPayloads(context.Background(), []runPayloadDeclaration{{Name: "secret", Path: source}})
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	jobID, err := ops.RecordQueuedJob(database, ops.QueueJobParams{Host: "host-alpha", WorkingDir: "/tmp/project", Command: "true", Payloads: payloads})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueByID(database, jobID); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetJobPayload(database, jobID, "secret")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := artifacts.LocalPathFromStored(row.StoredPath)
	got, err := os.ReadFile(stored)
	if err != nil || string(got) != "stable bytes" {
		t.Fatalf("stored payload = %q, %v", got, err)
	}
}

func TestPayloadReceiptInfoAndArtifactCatExposeMetadataNotContents(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	content := []byte("do-not-print-this-secret")
	source := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	stored, size, digest, err := artifacts.CapturePayload(source)
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	jobID, err := ops.RecordQueuedJob(database, ops.QueueJobParams{
		Host: "host-alpha", WorkingDir: "/tmp/project", Command: "true",
		Payloads: []db.JobPayload{{Name: "config", StoredPath: stored, SizeBytes: size, SHA256: digest, R2Key: "assets/" + digest}},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetJobByID(database, jobID)
	receipt, err := runReceiptForJob(database, job, "queued", false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Payloads) != 1 || receipt.Payloads[0].Name != "config" || receipt.Payloads[0].SizeBytes != int64(len(content)) || receipt.Payloads[0].SHA256 != digest {
		t.Fatalf("receipt payloads = %+v", receipt.Payloads)
	}
	var info bytes.Buffer
	if err := printJobPayloads(&info, database, jobID); err != nil {
		t.Fatal(err)
	}
	infoText := info.String()
	for _, want := range []string{"config", fmt.Sprintf("%d bytes", len(content)), digest, "artifact get", "payload:config"} {
		if !strings.Contains(infoText, want) {
			t.Errorf("info missing %q: %s", want, infoText)
		}
	}
	if strings.Contains(infoText, string(content)) {
		t.Fatalf("info leaked payload contents: %s", infoText)
	}

	var cat bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&cat)
	if err := deliverArtifactToken(command, database, jobID, job, "payload:config", false, true); err != nil {
		t.Fatalf("artifact cat payload: %v", err)
	}
	if !bytes.Equal(cat.Bytes(), content) {
		t.Fatalf("cat bytes = %q", cat.Bytes())
	}
	getPath := filepath.Join(t.TempDir(), "retrieved-config")
	previousOutput := artifactOutput
	artifactOutput = getPath
	t.Cleanup(func() { artifactOutput = previousOutput })
	var getOutput bytes.Buffer
	getCommand := &cobra.Command{}
	getCommand.SetOut(&getOutput)
	if err := deliverArtifactToken(getCommand, database, jobID, job, "payload:config", false, false); err != nil {
		t.Fatalf("artifact get payload: %v", err)
	}
	gotFile, err := os.ReadFile(getPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotFile, content) {
		t.Fatalf("get bytes = %q", gotFile)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256(content))
	if digest != wantDigest {
		t.Fatalf("digest = %s, want %s", digest, wantDigest)
	}
}

func TestQueueJobDependencyPathPersistsPayload(t *testing.T) {
	database := db.SetupTestDB(t)
	digest := strings.Repeat("a", 64)
	payload := db.JobPayload{
		Name: "config", StoredPath: "payloads/" + digest, SizeBytes: 7,
		SHA256: digest, R2Key: "assets/" + digest,
	}
	result, err := queueJob(database, queueJobOptions{
		Host: "host-alpha", WorkingDir: t.TempDir(), Command: "true",
		Payloads: []db.JobPayload{payload},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.ListJobPayloads(database, result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != payload.Name || got[0].SHA256 != digest {
		t.Fatalf("persisted payloads = %+v", got)
	}
}

func TestRunRunPersistsPayloadForDependencyJob(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 123, "host-alpha", "/tmp/dependency", "true", "", ""); err != nil {
		t.Fatal(err)
	}
	database.Close()
	inventory.UseTestHosts(t)

	dir := t.TempDir()
	source := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(source, []byte("review this"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetRunGlobals(t)
	runDir = dir
	runHost = "host-alpha"
	runAfterRaw = "123"
	runNoSync = true
	runPayloads = []string{"prompt=" + source}
	store := &memoryPayloadStore{objects: make(map[string][]byte)}
	previousStore := buildRunPayloadObjectStore
	previousProbe := validateRentalJobImageFunc
	buildRunPayloadObjectStore = func() (runPayloadObjectStore, error) { return store, nil }
	validateRentalJobImageFunc = func(context.Context, *config.Config, string, string) error { return nil }
	t.Cleanup(func() {
		buildRunPayloadObjectStore = previousStore
		validateRentalJobImageFunc = previousProbe
	})

	command := newRunTestCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	if err := runRun(command, []string{"python train.py"}); err != nil {
		t.Fatalf("runRun: %v\noutput:\n%s", err, output.String())
	}

	readDB, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer readDB.Close()
	var jobID int64
	if err := readDB.QueryRow(`SELECT job_id FROM job_payloads WHERE name = 'prompt'`).Scan(&jobID); err != nil {
		t.Fatalf("read payload job: %v", err)
	}
	payloads, err := db.ListJobPayloads(readDB, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 || payloads[0].Name != "prompt" {
		t.Fatalf("payloads = %+v", payloads)
	}
}
