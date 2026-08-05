package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	srcsync "github.com/osteele/weft/internal/sync"
)

func TestPrintSourceRootsPreviewShowsOriginsAndWarnings(t *testing.T) {
	source := &db.JobSourceMetadata{
		Roots: []db.JobSourceRootMetadata{
			{
				MountBasename: "mental-spaces",
				MountRel:      ".",
				Hash:          "a1b2c3d4ffff",
				Origins:       []string{srcsync.SourceRootOriginProject},
			},
			{
				MountBasename: "research-expkit",
				MountRel:      "../research-expkit",
				Hash:          "e5f6a7b8ffff",
				Origins:       []string{srcsync.SourceRootOriginExplicit, srcsync.SourceRootOriginDerived},
			},
		},
		Warnings: []string{"derived path dependency \"../shared\" from tool.uv.sources in train.py resolves outside the project but is not a sibling; it will not be synced"},
	}

	var out bytes.Buffer
	printSourceRootsPreview(&out, source, "/workspace/mental-spaces")
	got := out.String()
	for _, want := range []string{
		"../research-expkit",
		"/workspace/research-expkit",
		"(explicit, derived from tool.uv.sources)",
		"Warning: derived path dependency",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preview output missing %q:\n%s", want, got)
		}
	}
}
