package vastai

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An executable held open for writing cannot be exec'd: the kernel answers
// ETXTBSY. That is the condition a forked child creates for the microseconds
// between fork and exec, and it is what failed CI four times on
// TestSearchOffersPostFiltersMinCUDA and
// TestCreateInstanceSuccessWithoutContractIDIsProviderRejected. Holding the
// descriptor open here reproduces it deterministically instead of waiting for
// the race to land.
func TestWaitUntilExecutableWaitsOutATextFileBusyWriter(t *testing.T) {
	t.Parallel()
	stub := filepath.Join(t.TempDir(), "vastai")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	writer, err := os.OpenFile(stub, os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatalf("hold stub open for writing: %v", err)
	}
	defer writer.Close()

	// The premise: while that descriptor is open, exec is refused. Without it
	// the rest of this test would pass against any implementation.
	if err := exec.Command(stub).Run(); !errors.Is(err, syscall.ETXTBSY) {
		t.Skipf("platform does not report ETXTBSY for an open-for-write executable: %v", err)
	}
	if err := waitUntilExecutable(stub, 50*time.Millisecond); err == nil {
		t.Fatal("waitUntilExecutable reported a busy stub as ready")
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		writer.Close()
	}()
	if err := waitUntilExecutable(stub, 5*time.Second); err != nil {
		t.Fatalf("waitUntilExecutable did not wait out the writer: %v", err)
	}
}

// The probe must not consume the one response a stub is written to give, and
// must not disturb what it records about its arguments.
func TestProbeGuardedStubAnswersNormallyAndRecordsRealArgsOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	argsFile := filepath.Join(dir, "args.txt")
	writeExecutableStub(t, stub, "#!/bin/sh\nprintf '%s\\n' \"$@\" >>"+argsFile+"\nprintf 'payload\\n'\n")

	out, err := exec.Command(stub, "search", "offers").Output()
	if err != nil {
		t.Fatalf("run stub: %v", err)
	}
	if strings.TrimSpace(string(out)) != "payload" {
		t.Fatalf("stub output = %q, want its payload", out)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	if strings.Contains(string(recorded), stubProbeFlag) {
		t.Fatalf("probe leaked into the stub's recorded arguments: %q", recorded)
	}
	if strings.TrimSpace(string(recorded)) != "search\noffers" {
		t.Fatalf("recorded args = %q, want the caller's arguments only", recorded)
	}
}
