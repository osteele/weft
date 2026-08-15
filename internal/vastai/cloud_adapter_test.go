package vastai

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
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

func TestCloudClientSearchOffersMapsDatacenterDriver(t *testing.T) {
	nvlinkBandwidth := 478.116
	client := NewCloudClient(&MockClient{
		SearchOffersFunc: func(c OfferConstraints) ([]Offer, error) {
			return []Offer{{
				ID:               123,
				GPUName:          "RTX 3090",
				NumGPUs:          1,
				GPUMemGB:         24,
				CostPerHour:      0.45,
				DatacenterDriver: true,
				NVLinkBandwidth:  &nvlinkBandwidth,
			}}, nil
		},
	})

	offers, err := client.SearchOffers(cloud.OfferConstraints{GPUClass: "RTX 3090"})
	if err != nil {
		t.Fatalf("SearchOffers: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("offers len = %d, want 1", len(offers))
	}
	if !offers[0].DatacenterDriver {
		t.Fatalf("DatacenterDriver = false, want true")
	}
	if offers[0].NVLinkBandwidth == nil || *offers[0].NVLinkBandwidth != nvlinkBandwidth {
		t.Fatalf("NVLinkBandwidth = %v, want %v", offers[0].NVLinkBandwidth, nvlinkBandwidth)
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

// runSelfDestructCommand returns the command string for inspection.
func runSelfDestructCommand(t *testing.T, userKey string) string {
	t.Helper()
	return (&CloudClient{}).selfDestructCmdWithKey("12345", userKey)
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

// TestSelfDestructCmdDoesNotEmbedKey verifies the literal user key is passed
// through the WEFT_VAST_API_KEY environment variable and is not hardcoded
// inside the _d helper (e.g., in the curl Authorization header or URL).
func TestSelfDestructCmdDoesNotEmbedKey(t *testing.T) {
	t.Parallel()
	key := "vast-api-key-with-\"quotes\\and$special"
	cmd := runSelfDestructCommand(t, key)
	if !strings.Contains(cmd, "WEFT_VAST_API_KEY=") {
		t.Fatalf("SelfDestructCmd does not set WEFT_VAST_API_KEY:\n%s", cmd)
	}
	if !strings.Contains(cmd, "Authorization: Bearer ${WEFT_VAST_API_KEY:-}") {
		t.Fatalf("SelfDestructCmd does not read the key from WEFT_VAST_API_KEY:\n%s", cmd)
	}
	// The body of _d must reference the variable, not the literal key.
	funcStart := strings.Index(cmd, "_d() {")
	if funcStart < 0 {
		t.Fatal("_d function not found in command")
	}
	if strings.Contains(cmd[funcStart:], key) {
		t.Fatalf("literal API key appears inside _d body:\n%s", cmd[funcStart:])
	}
}

// TestSelfDestructCmdHandlesKeyWithShellMetacharacters verifies that a key
// containing characters that would break a naive interpolated command still
// authorizes the fallback curl request.
func TestSelfDestructCmdHandlesKeyWithShellMetacharacters(t *testing.T) {
	t.Parallel()
	key := "user-key-with-\"quotes\\and'apostrophes$pecial"
	_, stderr, err := runSelfDestruct(t, key, nil)
	if err != nil {
		t.Fatalf("expected user-key path to run; err=%v stderr=%s", err, stderr)
	}
}

// TestSelfDestructCmdUsesContainerKeyOverride verifies that when
// CONTAINER_API_KEY is set and authorized, the container key is used instead of
// the user key, even though the user key is configured in WEFT_VAST_API_KEY.
func TestSelfDestructCmdUsesContainerKeyOverride(t *testing.T) {
	t.Parallel()
	_, stderr, err := runSelfDestruct(t, "wrong-key", map[string]string{
		"CONTAINER_API_KEY": "user-key",
		"CONTAINER_ID":      "12345",
	})
	if err != nil {
		t.Fatalf("expected container-key path to succeed; err=%v stderr=%s", err, stderr)
	}
}
