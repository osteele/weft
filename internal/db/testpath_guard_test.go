package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTestBinaryNeverResolvesToSharedDatabase guards wb75. Opening the
// database applies pending migrations, so a test in a tree carrying an
// unreleased migration advanced the shared schema past what the installed
// binary understood and broke every weft command on the machine. A test binary
// must resolve somewhere private, whether or not the test remembered
// SetDBPath.
func TestTestBinaryNeverResolvesToSharedDatabase(t *testing.T) {
	sharedDir := filepath.Clean(filepath.Dir(sharedDBFile))
	for name, path := range map[string]string{"jobs": dbPath, "bugs": bugDBPath} {
		if path == "" {
			t.Fatalf("%s path is empty", name)
		}
		if strings.HasPrefix(filepath.Clean(path), sharedDir+string(filepath.Separator)) {
			t.Fatalf("%s path %s is inside the shared config dir %s", name, path, sharedDir)
		}
	}
}

// The detection must key on the test binary itself, not on anything a test sets
// up, since it runs during package initialization. Both signals matter: `go
// test` always produces a `.test` executable, while `go test -c -o name`
// produces one that is only recognizable from its harness flags.
func TestLooksLikeTestBinary(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		args []string
		want bool
	}{
		{name: "go test executable", exe: "/tmp/go-build123/b001/db.test", want: true},
		{name: "windows go test executable", exe: `C:\tmp\db.test.exe`, want: true},
		{name: "renamed test binary", exe: "/tmp/checks", args: []string{"-test.run=TestX"}, want: true},
		{name: "renamed test binary, long flag", exe: "/tmp/checks", args: []string{"--test.v=true"}, want: true},
		{name: "installed weft", exe: "/usr/local/bin/weft", args: []string{"status", "wj1"}, want: false},
		{name: "weft subcommand named for a test", exe: "/usr/local/bin/weft", args: []string{"run", "test.py"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeTestBinary(tc.exe, tc.args); got != tc.want {
				t.Fatalf("looksLikeTestBinary(%q, %q) = %v, want %v", tc.exe, tc.args, got, tc.want)
			}
		})
	}
}

// TestDevBuildMigrationGuardScope pins what the guard leaves alone. It must not
// refuse on unknown evidence: a machine with no installed weft, or a developer
// pointing at their own database, is not the hazard.
func TestDevBuildMigrationGuardScope(t *testing.T) {
	t.Setenv(AllowDevMigrationEnv, "")

	// A private database is nobody else's problem, even from a dev build.
	private := filepath.Join(t.TempDir(), "jobs.db")
	if err := checkDevBuildMayMigrate(private); err != nil {
		t.Fatalf("guard refused a private database: %v", err)
	}

	// No weft on PATH is unknown, not mismatched.
	t.Setenv("PATH", t.TempDir())
	if err := checkDevBuildMayMigrate(sharedDBFile); err != nil {
		t.Fatalf("guard refused with no installed weft on PATH: %v", err)
	}

	// The override is honoured for the shared path.
	t.Setenv(AllowDevMigrationEnv, "1")
	t.Setenv("PATH", installedWeftStub(t))
	if err := checkDevBuildMayMigrate(sharedDBFile); err != nil {
		t.Fatalf("guard refused the shared database despite %s: %v", AllowDevMigrationEnv, err)
	}
}

// TestDevBuildMigrationGuardRefusesForeignBuild reproduces wb75 itself: a
// binary that is not the installed weft, about to migrate the shared database.
// PATH points at a stand-in so the refusal does not depend on whether this
// machine happens to have weft installed. The message must name both binaries
// and the way out, because its reader is mid-incident.
func TestDevBuildMigrationGuardRefusesForeignBuild(t *testing.T) {
	t.Setenv(AllowDevMigrationEnv, "")
	t.Setenv("PATH", installedWeftStub(t))

	err := checkDevBuildMayMigrate(sharedDBFile)
	if err == nil {
		t.Fatal("guard allowed a build that is not the installed weft to migrate the shared database")
	}
	for _, want := range []string{"running:", "installed:", "database:", AllowDevMigrationEnv, "just install"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal message missing %q: %v", want, err)
		}
	}
}

// TestDevBuildMigrationGuardAllowsInstalledBinaryViaSymlink pins samePath's
// symlink resolution, which is what keeps the guard from refusing the installed
// binary reached under another name.
func TestDevBuildMigrationGuardAllowsInstalledBinaryViaSymlink(t *testing.T) {
	t.Setenv(AllowDevMigrationEnv, "")
	running, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve this executable: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(running, filepath.Join(dir, "weft")); err != nil {
		t.Fatalf("symlink stand-in for installed weft: %v", err)
	}
	t.Setenv("PATH", dir)

	if err := checkDevBuildMayMigrate(sharedDBFile); err != nil {
		t.Fatalf("guard refused the running binary reached through a symlink: %v", err)
	}
}

// installedWeftStub returns a PATH directory holding an executable named weft
// that is not this process.
func installedWeftStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "weft"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write installed weft stand-in: %v", err)
	}
	return dir
}
