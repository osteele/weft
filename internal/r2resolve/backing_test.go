package r2resolve

import (
	"testing"

	"github.com/osteele/weft/internal/artifacts"
)

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
