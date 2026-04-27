package vastai

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubCurl writes a fake `curl` into dir that mimics the subset of behavior
// SelfDestructCmd relies on: prints the HTTP status code to stdout (matching
// `-w '%{http_code}'`) and returns 401 unless the Authorization header carries
// `user-key`, in which case it returns 200. It exits 0 in both cases — like
// real curl without `-f`.
func stubCurl(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "curl")
	script := `#!/bin/sh
auth=""
for arg in "$@"; do
  case "$arg" in
    "Authorization: Bearer "*) auth="$arg" ;;
  esac
done
case "$auth" in
  *user-key*) printf 200 ;;
  *)          printf 401 ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub curl: %v", err)
	}
}

// runSelfDestruct builds the bash command from selfDestructCmdWithKey and
// runs it under a stubbed curl. Any keys in extraEnv (PATH, CONTAINER_API_KEY,
// CONTAINER_ID, ...) override or supplement the inherited env.
func runSelfDestruct(t *testing.T, userKey string, extraEnv map[string]string) (stdout, stderr string, err error) {
	t.Helper()
	dir := t.TempDir()
	stubCurl(t, dir)
	cmdStr := (&CloudClient{}).selfDestructCmdWithKey("12345", userKey)
	cmd := exec.Command("bash", "-c", cmdStr)
	overridden := map[string]bool{}
	env := []string{"PATH=" + dir + ":" + os.Getenv("PATH")}
	overridden["PATH"] = true
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
		overridden[k] = true
	}
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i >= 0 && overridden[e[:i]] {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// TestSelfDestructCmdFallsBackOnAuthFailure exercises the wi1517 scenario:
// CONTAINER_API_KEY is set but unauthorized. The composite must still succeed
// via the user-key fallback — the previous ${VAR:-fallback} form only fell
// back when the var was unset, leaving interruptible instances stranded.
func TestSelfDestructCmdFallsBackOnAuthFailure(t *testing.T) {
	t.Parallel()
	_, stderr, err := runSelfDestruct(t, "user-key", map[string]string{
		"CONTAINER_API_KEY": "bogus-per-instance-key",
		"CONTAINER_ID":      "12345",
	})
	if err != nil {
		t.Fatalf("expected fallback to user key to succeed; err=%v stderr=%s", err, stderr)
	}
}

func TestSelfDestructCmdUsesUserKeyWhenContainerKeyUnset(t *testing.T) {
	t.Parallel()
	_, stderr, err := runSelfDestruct(t, "user-key", nil)
	if err != nil {
		t.Fatalf("expected user-key path to run; err=%v stderr=%s", err, stderr)
	}
}

// When both keys are rejected, the helper must record HTTP=<code> and url=...
// to stderr so termination-intent.json LastError distinguishes 401/403/404/5xx.
func TestSelfDestructCmdEmitsHTTPDiagnostic(t *testing.T) {
	t.Parallel()
	_, stderr, err := runSelfDestruct(t, "wrong-key", map[string]string{
		"CONTAINER_API_KEY": "also-wrong",
		"CONTAINER_ID":      "12345",
	})
	if err == nil {
		t.Fatalf("expected non-zero exit when both keys rejected; stderr=%s", stderr)
	}
	for _, want := range []string{"HTTP=401", "url=https://console.vast.ai/api/v0/instances/12345/"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, stderr)
		}
	}
}
