package sync

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildOutputFileSyncArgsUsesFilesFrom(t *testing.T) {
	args := BuildOutputFileSyncArgs("cool30", "/remote/project", "/local/project")
	if !slices.Contains(args, "--files-from=-") {
		t.Fatalf("args missing --files-from=-: %v", args)
	}
	if got := args[len(args)-2]; !strings.HasSuffix(got, ":/remote/project/") {
		t.Fatalf("source = %q, want remote project root", got)
	}
	if got := args[len(args)-1]; got != "/local/project/" {
		t.Fatalf("destination = %q, want /local/project/", got)
	}
}

func TestCleanOutputFileListRejectsUnsafePaths(t *testing.T) {
	got := cleanOutputFileList([]string{
		" output/result.json ",
		"/output/absolute-prefix.csv",
		"../outside.txt",
		"output/../outside.txt",
		"",
	})
	want := []string{"output/result.json", "output/absolute-prefix.csv"}
	if !slices.Equal(got, want) {
		t.Fatalf("cleaned = %#v, want %#v", got, want)
	}
}
