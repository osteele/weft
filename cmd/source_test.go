package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2keys"
)

type fakeSourceStore struct {
	objects map[string][]byte
}

func (s *fakeSourceStore) GetObject(_ context.Context, key string) ([]byte, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeSourceStore) GetObjectReader(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestParseBootstrapSources(t *testing.T) {
	script := `
rclone copyto "r2:$R2_BUCKET/sources/aaa.tar.gz" /tmp/src.tar.gz && tar xzf /tmp/src.tar.gz -C "/workspace/proj-a" && rm -f /tmp/src.tar.gz
rclone copyto "r2:$R2_BUCKET/sources/bbb.tar.gz" /tmp/src.tar.gz && tar xzf /tmp/src.tar.gz -C "/workspace/proj b" && rm -f /tmp/src.tar.gz
`
	got, err := parseBootstrapSources(script)
	if err != nil {
		t.Fatalf("parseBootstrapSources: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].R2Key != "sources/aaa.tar.gz" || got[0].RemoteDir != "/workspace/proj-a" {
		t.Fatalf("first mapping = %+v", got[0])
	}
	if got[1].R2Key != "sources/bbb.tar.gz" || got[1].RemoteDir != "/workspace/proj b" {
		t.Fatalf("second mapping = %+v", got[1])
	}
}

func TestResolveJobSource_MatchesManifestDirectory(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/Users/osteele/code/proj-b", "uv run python scripts/train.py", "source")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "runpod"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	attempts, err := db.ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	runID := attempts[0].ID

	store := &fakeSourceStore{objects: map[string][]byte{
		r2keys.BootstrapScript(launchID): []byte(`
rclone copyto "r2:$R2_BUCKET/sources/proj-a.tar.gz" /tmp/src.tar.gz && tar xzf /tmp/src.tar.gz -C "/workspace/proj-a" && rm -f /tmp/src.tar.gz
rclone copyto "r2:$R2_BUCKET/sources/proj-b.tar.gz" /tmp/src.tar.gz && tar xzf /tmp/src.tar.gz -C "/workspace/proj-b" && rm -f /tmp/src.tar.gz
`),
		r2keys.CampaignManifest(launchID): []byte(`{"jobs":[{"id":` + itoa64(jobID) + `,"run_id":` + itoa64(runID) + `,"cmd":"uv run python scripts/train.py","dir":"/workspace/proj-b"}]}`),
	}}

	source, err := resolveJobSource(context.Background(), database, store, jobID, 0)
	if err != nil {
		t.Fatalf("resolveJobSource: %v", err)
	}
	if source.Mapping.R2Key != "sources/proj-b.tar.gz" {
		t.Fatalf("R2Key = %q, want proj-b tarball", source.Mapping.R2Key)
	}
}

func TestResolveJobSource_RejectsNonCloudAttempt(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "cool30", "/tmp/project", "python train.py", "source")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	_, err = resolveJobSource(context.Background(), database, &fakeSourceStore{}, jobID, 0)
	if err == nil {
		t.Fatal("resolveJobSource returned nil error, want non-cloud failure")
	}
	if !strings.Contains(err.Error(), "source inspection is only available for cloud attempts") {
		t.Fatalf("error = %q", err)
	}
}

func TestListSourceTarball_Prefix(t *testing.T) {
	tarball := makeSourceTarball(t, map[string]string{
		"scripts/train.py":  "print('ok')\n",
		"scripts2/skip.py":  "skip\n",
		"README.md":         "readme\n",
		"scripts/nested.sh": "echo ok\n",
	})
	var out bytes.Buffer
	if err := listSourceTarball(bytes.NewReader(tarball), "scripts", &out); err != nil {
		t.Fatalf("listSourceTarball: %v", err)
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{"scripts/train.py", "scripts/nested.sh"}
	slices.Sort(got)
	slices.Sort(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCatSourceTarballFile(t *testing.T) {
	tarball := makeSourceTarball(t, map[string]string{
		"scripts/train.py": "print('ok')\n",
	})
	var out bytes.Buffer
	if err := catSourceTarballFile(bytes.NewReader(tarball), "./scripts/train.py", &out); err != nil {
		t.Fatalf("catSourceTarballFile: %v", err)
	}
	if out.String() != "print('ok')\n" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestInferCommandSourcePaths(t *testing.T) {
	got := inferCommandSourcePaths(`CUDA_VISIBLE_DEVICES=0 uv run python -u scripts/train.py --device cuda && bash scripts/post.sh`)
	want := []string{"scripts/train.py", "scripts/post.sh"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func makeSourceTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		data := []byte(content)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatalf("WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("Close gzip: %v", err)
	}
	return buf.Bytes()
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
