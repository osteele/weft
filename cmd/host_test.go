package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
)

func TestHostListIncludesInventoryHosts(t *testing.T) {
	setTestHostInventory(t, []inventory.HostSpec{
		{
			Name:     "host-alpha",
			OS:       "linux",
			Arch:     "amd64",
			CPUCores: 64,
			Memory:   "256GB",
			GPUs: []inventory.GPUSpec{
				{Name: "A100 80GB PCIe", Memory: "80GB", Indices: []int{0, 1}},
			},
		},
	})
	db.SetupTestDB(t)

	out := captureStdout(t, func() {
		if err := runHostList(nil, nil); err != nil {
			t.Fatalf("runHostList: %v", err)
		}
	})

	line := findHostListLine(t, out, "host-alpha")
	if !strings.Contains(line, "linux/amd64") {
		t.Fatalf("inventory row missing OS/arch: %q", line)
	}
	if !strings.Contains(line, "64 cores") {
		t.Fatalf("inventory row missing CPU count: %q", line)
	}
	if !strings.Contains(line, "256GB") {
		t.Fatalf("inventory row missing memory: %q", line)
	}
}

func TestHostListIncludesRecentCachedNonInventoryHosts(t *testing.T) {
	setTestHostInventory(t, []inventory.HostSpec{
		{
			Name:     "cool100",
			OS:       "linux",
			Arch:     "amd64",
			CPUCores: 8,
			Memory:   "64GB",
		},
	})
	database := db.SetupTestDB(t)
	now := time.Now()
	if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
		Name:        "cool30",
		Arch:        "Linux x86_64",
		CPUCount:    32,
		MemTotal:    "128G",
		GPUsJSON:    `[{"Index":0,"Name":"RTX 3090","MemTotal":"24576 MiB"}]`,
		LastUpdated: now.Unix(),
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runHostList(nil, nil); err != nil {
			t.Fatalf("runHostList: %v", err)
		}
	})

	line := findHostListLine(t, out, "cool30")
	if !strings.Contains(line, "linux/amd64") {
		t.Fatalf("cached row missing OS/arch: %q", line)
	}
	if !strings.Contains(line, "32 cores") {
		t.Fatalf("cached row missing CPU count: %q", line)
	}
	if !strings.Contains(line, "128G") {
		t.Fatalf("cached row missing memory: %q", line)
	}
	if !strings.Contains(line, "RTX 3090") {
		t.Fatalf("cached row missing GPU summary: %q", line)
	}

	cool30Idx := strings.Index(out, "cool30")
	cool100Idx := strings.Index(out, "cool100")
	if cool30Idx == -1 || cool100Idx == -1 || cool30Idx >= cool100Idx {
		t.Fatalf("expected natural sort order cool30 before cool100:\n%s", out)
	}
}

func TestHostListIncludesRecentSyncOnlyHostsWithUnknownPlaceholders(t *testing.T) {
	setTestHostInventory(t, nil)
	database := db.SetupTestDB(t)
	if err := db.RecordHostSync(database, "recent-sync-host", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("RecordHostSync: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runHostList(nil, nil); err != nil {
			t.Fatalf("runHostList: %v", err)
		}
	})

	line := findHostListLine(t, out, "recent-sync-host")
	if !strings.Contains(line, "unknown/unknown") || !strings.Contains(line, "unknown") {
		t.Fatalf("expected unknown placeholders in sync-only row: %q", line)
	}
}

func TestHostListOmitsStaleNonInventoryHosts(t *testing.T) {
	setTestHostInventory(t, nil)
	database := db.SetupTestDB(t)
	if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
		Name:        "stale-host",
		Arch:        "Linux x86_64",
		CPUCount:    16,
		MemTotal:    "64G",
		LastUpdated: time.Now().Add(-72 * time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runHostList(nil, nil); err != nil {
			t.Fatalf("runHostList: %v", err)
		}
	})

	if hostListHasHost(out, "stale-host") {
		t.Fatalf("stale host should be omitted:\n%s", out)
	}
}

func TestHostListIncludesActiveOnPremHostsWithoutRecentSyncOrCache(t *testing.T) {
	setTestHostInventory(t, nil)
	database := db.SetupTestDB(t)
	if _, err := db.RecordQueuedWithGPU(database, "active-host", "/tmp/project", "python train.py", "train", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runHostList(nil, nil); err != nil {
			t.Fatalf("runHostList: %v", err)
		}
	})

	line := findHostListLine(t, out, "active-host")
	if !strings.Contains(line, "unknown/unknown") {
		t.Fatalf("active host without cached specs should use unknown placeholders: %q", line)
	}
}

func TestHostListExcludesCloudHosts(t *testing.T) {
	setTestHostInventory(t, nil)
	database := db.SetupTestDB(t)
	cloudHost := db.LaunchHost(17)
	if err := db.RecordHostSync(database, cloudHost, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("RecordHostSync: %v", err)
	}
	if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
		Name:        cloudHost,
		Arch:        "Linux x86_64",
		CPUCount:    8,
		MemTotal:    "32G",
		LastUpdated: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runHostList(nil, nil); err != nil {
			t.Fatalf("runHostList: %v", err)
		}
	})

	if hostListHasHost(out, cloudHost) {
		t.Fatalf("cloud host should be omitted:\n%s", out)
	}
}

func TestHostListDeduplicatesInventoryAndRecentStateAndPrefersInventory(t *testing.T) {
	setTestHostInventory(t, []inventory.HostSpec{
		{
			Name:     "cool30",
			OS:       "linux",
			Arch:     "amd64",
			CPUCores: 16,
			Memory:   "64GB",
			GPUs: []inventory.GPUSpec{
				{Name: "RTX 3090", Memory: "24GB", Indices: []int{0}},
			},
		},
	})
	database := db.SetupTestDB(t)
	if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
		Name:        "cool30",
		Arch:        "Linux x86_64",
		CPUCount:    999,
		MemTotal:    "999G",
		LastUpdated: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}
	if err := db.RecordHostSync(database, "cool30", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("RecordHostSync: %v", err)
	}
	if _, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "train", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runHostList(nil, nil); err != nil {
			t.Fatalf("runHostList: %v", err)
		}
	})

	if got := countHostListRows(out, "cool30"); got != 1 {
		t.Fatalf("cool30 row count = %d, want 1\n%s", got, out)
	}

	line := findHostListLine(t, out, "cool30")
	if !strings.Contains(line, "16 cores") || strings.Contains(line, "999 cores") {
		t.Fatalf("expected inventory specs to win over cached specs: %q", line)
	}
	if !strings.Contains(line, "64GB") || strings.Contains(line, "999G") {
		t.Fatalf("expected inventory memory to win over cached specs: %q", line)
	}
}

func setTestHostInventory(t *testing.T, hosts []inventory.HostSpec) {
	t.Helper()

	cleanup := inventory.SetHosts(hosts)
	t.Cleanup(cleanup)
}

func scanHostLines(output, host string) []string {
	var matches []string
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == host {
			matches = append(matches, line)
		}
	}
	return matches
}

func findHostListLine(t *testing.T, output, host string) string {
	t.Helper()
	if lines := scanHostLines(output, host); len(lines) > 0 {
		return lines[0]
	}
	t.Fatalf("host %q not found in output:\n%s", host, output)
	return ""
}

func hostListHasHost(output, host string) bool {
	return len(scanHostLines(output, host)) > 0
}

func countHostListRows(output, host string) int {
	return len(scanHostLines(output, host))
}
