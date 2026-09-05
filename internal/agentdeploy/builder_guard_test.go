package agentdeploy

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/inventory"
)

// A build host receives a full source sync and runs compiles. Routing that onto
// a machine weft does not manage is how a routing node's disk filled with build
// artifacts for months without anything reporting it.
func TestBuilderHostMustBeInTheInventory(t *testing.T) {
	inventory.UseTestHosts(t)

	if reason := builderHostRefusal("host-alpha", false); reason != "" {
		t.Errorf("an inventory host was refused as a builder: %s", reason)
	}
	reason := builderHostRefusal("definitely-not-an-inventory-host", false)
	if reason == "" {
		t.Fatal("a host weft chose for itself, absent from the inventory, was accepted")
	}
	if !strings.Contains(reason, "inventory") {
		t.Errorf("refusal should name the inventory requirement: %s", reason)
	}

	// An operator who names a host in configuration has chosen it. That is
	// honoured even outside the inventory; the disk and toolchain probes are
	// what protect the host from a build it cannot run.
	if reason := builderHostRefusal("definitely-not-an-inventory-host", true); reason != "" {
		t.Errorf("an explicitly configured builder was refused: %s", reason)
	}
}

// A toolchain older than the module's go directive still builds, by downloading
// the required one first. That is a large per-cold-build cost on every cold
// cache, not a working configuration, so it is reported rather than paid
// silently.
func TestToolchainSatisfiesComparesVersions(t *testing.T) {
	cases := []struct {
		line     string
		required string
		ok       bool
	}{
		{"go version go1.25.7 linux/amd64", "1.25.7", true},
		{"go version go1.26.0 linux/amd64", "1.25.7", true},
		{"go version go1.24.2 linux/amd64", "1.25.7", false},
		{"go version go1.9.1 linux/amd64", "1.25.7", false},
		{"garbage", "1.25.7", true},
	}
	for _, tc := range cases {
		if got := toolchainSatisfies(tc.line, tc.required); got != tc.ok {
			t.Errorf("toolchainSatisfies(%q, %q) = %v, want %v", tc.line, tc.required, got, tc.ok)
		}
	}
}

// Version comparison must be numeric, or 1.9 sorts above 1.25 and an unusable
// toolchain passes the check.
func TestVersionComparisonIsNumericNotLexical(t *testing.T) {
	if !semverLess("1.9.1", "1.25.7") {
		t.Error("1.9.1 should compare below 1.25.7; a lexical compare gets this backwards")
	}
	if semverLess("1.25.7", "1.9.1") {
		t.Error("1.25.7 should not compare below 1.9.1")
	}
}

// A refused builder must not disable the builders beside it. Builders are
// independent, and treating one bad entry as fatal turns a degraded
// configuration into no builds at all.
func TestRefusedBuilderIsSkippedNotFatal(t *testing.T) {
	inventory.UseTestHosts(t)
	t.Setenv("WEFT_LINUX_BUILDER_HOST", "definitely-not-an-inventory-host")
	// Explicitly configured, so it is kept rather than skipped.
	t.Setenv("WEFT_FLY_BUILDER_APP", "test-app")
	t.Setenv("WEFT_FLY_BUILDER_MACHINE", "test-machine")

	builders, err := resolveBuilders("linux", "amd64")
	if err != nil {
		t.Fatalf("a refused ssh builder made builder resolution fail: %v", err)
	}
	if len(builders) < 2 {
		t.Fatalf("expected both builders, got %d: %+v", len(builders), builders)
	}
}
