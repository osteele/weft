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

func TestBuildOutputFileSyncArgsStripsTildeRemoteDir(t *testing.T) {
	// rsync does not shell-expand a leading "~/" after "host:"; it must be
	// stripped so the path is home-relative rather than a literal "~" dir.
	args := BuildOutputFileSyncArgs("cool30", "~/code/research/adjective-order", "/local/project")
	src := args[len(args)-2]
	if !strings.HasSuffix(src, ":code/research/adjective-order/") {
		t.Fatalf("source = %q, want tilde stripped to home-relative path", src)
	}
	if strings.Contains(src, "~") {
		t.Fatalf("source = %q still contains a literal tilde", src)
	}
}

func TestRemoteRsyncPath(t *testing.T) {
	cases := map[string]string{
		"~/code/x":     "code/x",
		"~/code/x/":    "code/x",
		"~":            ".",
		"/abs/path":    "/abs/path",
		"/abs/path/":   "/abs/path",
		"rel/path":     "rel/path",
		"~notexpanded": "~notexpanded", // only "~/" and bare "~" are home refs
	}
	for in, want := range cases {
		if got := remoteRsyncPath(in); got != want {
			t.Errorf("remoteRsyncPath(%q) = %q, want %q", in, got, want)
		}
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
