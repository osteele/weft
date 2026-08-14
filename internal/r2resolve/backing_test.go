package r2resolve

import (
	"context"
	"testing"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/r2"
)

type listedObjects struct{ objects []r2.ObjectInfo }

func (l listedObjects) ListObjects(context.Context, string) ([]r2.ObjectInfo, error) {
	return l.objects, nil
}

func TestListRunIDsNewestFirst(t *testing.T) {
	got, err := ListRunIDsFunc(context.Background(), listedObjects{objects: []r2.ObjectInfo{
		{Key: "jobs/7/runs/2/output/a"},
		{Key: "jobs/7/runs/11/output/a"},
		{Key: "jobs/7/runs/5/output/a"},
	}}, 7)
	if err != nil {
		t.Fatalf("ListRunIDsFunc: %v", err)
	}
	want := []int64{11, 5, 2}
	if len(got) != len(want) {
		t.Fatalf("run IDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("run IDs = %v, want %v", got, want)
		}
	}
}

func TestConventionOutputRelPath(t *testing.T) {
	tests := []struct {
		name       string
		manifest   artifacts.Manifest
		path       string
		outputDirs []string
		want       string
		ok         bool
	}{
		{name: "default output", path: "./output/checkpoints/../model.pt", want: "output/model.pt", ok: true},
		{name: "custom output", path: "results/model.pt", outputDirs: []string{"results/"}, want: "results/model.pt", ok: true},
		{name: "outside output", path: "checkpoints/model.pt"},
		{name: "absolute", path: "/tmp/output/model.pt"},
		{name: "escaping", path: "../output/model.pt"},
		{name: "backslash spelling", path: `output\model.pt`},
		{name: "custom artifact root", manifest: artifacts.Manifest{ArtifactRoot: "scratch"}, path: "output/model.pt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ConventionOutputRelPath(tc.manifest, tc.path, tc.outputDirs)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("ConventionOutputRelPath() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
