package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ssh"
)

// configureTestHosts points the ssh package at a fixed host config, matching
// what cmd/root.go does at startup, and restores the empty config after.
func configureTestHosts(t *testing.T, hosts map[string]config.HostConfig) *config.Config {
	t.Helper()
	cfg := &config.Config{Hosts: hosts}
	ssh.Configure(cfg)
	t.Cleanup(func() { ssh.Configure(&config.Config{}) })
	return cfg
}

func decodeHostListJSON(t *testing.T, rows []hostListRow) (map[string]any, string) {
	t.Helper()
	var buf bytes.Buffer
	if err := writeHostListJSON(&buf, rows); err != nil {
		t.Fatalf("writeHostListJSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal %q: %v", buf.String(), err)
	}
	return doc, buf.String()
}

func TestHostListJSONCarriesVersionedEnvelope(t *testing.T) {
	doc, raw := decodeHostListJSON(t, []hostListRow{{Type: "host", Name: "host-alpha"}})

	if doc["kind"] != "host_list" {
		t.Errorf("kind = %v, want host_list; got %s", doc["kind"], raw)
	}
	// A consumer that pins the version refuses anything else, so a silent bump
	// takes its daemon offline. See HostListMachineSurface in
	// specs/inventory-placement.allium.
	if version, ok := doc["version"].(float64); !ok || version != 1 {
		t.Errorf("version = %v, want 1; got %s", doc["version"], raw)
	}
}

func TestHostListJSONPublishesSSHTargetWithItsIdentityFile(t *testing.T) {
	testHostConfig := configureTestHosts(t, map[string]config.HostConfig{
		"host-alpha": {
			SSHUser:         "agent",
			SSHIdentityFile: "/keys/agent_ed25519",
		},
	})

	rows := []hostListRow{hostListRowFromSpec(inventory.HostSpec{
		Name:         "host-alpha",
		Capabilities: []string{"agent:codex", "agent:claude"},
	})}
	applyHostConfigFields(rows, testHostConfig)
	doc, raw := decodeHostListJSON(t, rows)

	hosts, _ := doc["hosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("hosts = %d, want 1; got %s", len(hosts), raw)
	}
	host, _ := hosts[0].(map[string]any)

	if host["ssh_target"] != "agent@host-alpha" {
		t.Errorf("ssh_target = %v, want agent@host-alpha", host["ssh_target"])
	}
	// The target alone selects a different key through ssh_config on at least
	// one real host, so publishing it without the identity file hands the
	// consumer a command that fails.
	if host["ssh_identity_file"] != "/keys/agent_ed25519" {
		t.Errorf("ssh_identity_file = %v, want /keys/agent_ed25519", host["ssh_identity_file"])
	}

	capabilities, _ := host["capabilities"].([]any)
	if len(capabilities) != 2 || capabilities[0] != "agent:codex" || capabilities[1] != "agent:claude" {
		t.Errorf("capabilities = %v, want [agent:codex agent:claude]", host["capabilities"])
	}
}

func TestHostListJSONPublishesEffectiveInventoryCapabilities(t *testing.T) {
	testHostConfig := configureTestHosts(t, map[string]config.HostConfig{
		"host-alpha": {SSHUser: "agent"},
	})
	rows := []hostListRow{hostListRowFromSpec(inventory.HostSpec{
		Name:         "host-alpha",
		Capabilities: []string{"agent:yaml-only"},
	})}
	applyHostConfigFields(rows, testHostConfig)
	doc, raw := decodeHostListJSON(t, rows)

	hosts, _ := doc["hosts"].([]any)
	host, _ := hosts[0].(map[string]any)
	capabilities, _ := host["capabilities"].([]any)
	if len(capabilities) != 1 || capabilities[0] != "agent:yaml-only" {
		t.Fatalf("capabilities = %v, want effective inventory set; got %s", capabilities, raw)
	}
}

func TestHostListJSONKeepsDisprovedCapabilityAdvisory(t *testing.T) {
	observedAt := time.Unix(1_787_700_000, 0)
	rows := []hostListRow{{
		Type:         "host",
		Name:         "studio",
		Capabilities: []string{"agent:claude", "agent:codex"},
		CapabilityObservations: []db.HostCapabilityObservation{{
			Host:       "studio",
			Label:      "agent:claude",
			Source:     "probe:agent-review",
			Observed:   false,
			Detail:     "loggedIn=false",
			ObservedAt: observedAt,
		}},
	}}
	doc, raw := decodeHostListJSON(t, rows)

	if version, _ := doc["version"].(float64); version != 1 {
		t.Fatalf("additive observation changed version to %v: %s", version, raw)
	}
	hosts, _ := doc["hosts"].([]any)
	host, _ := hosts[0].(map[string]any)
	capabilities, _ := host["capabilities"].([]any)
	if len(capabilities) != 2 || capabilities[0] != "agent:claude" || capabilities[1] != "agent:codex" {
		t.Fatalf("absent observation filtered declared capabilities: %s", raw)
	}
	observations, _ := host["capability_observations"].([]any)
	if len(observations) != 1 {
		t.Fatalf("capability_observations = %v: %s", host["capability_observations"], raw)
	}
	observation, _ := observations[0].(map[string]any)
	if observation["label"] != "agent:claude" || observation["observed"] != false || observation["source"] != "probe:agent-review" || observation["observed_at"] != float64(observedAt.Unix()) || observation["detail"] != "loggedIn=false" {
		t.Fatalf("observation = %v: %s", observation, raw)
	}
}

func TestHostListJSONOmitsCapabilityObservationsWhenNoneExist(t *testing.T) {
	doc, raw := decodeHostListJSON(t, []hostListRow{{Type: "host", Name: "host-alpha"}})
	hosts, _ := doc["hosts"].([]any)
	host, _ := hosts[0].(map[string]any)
	if _, present := host["capability_observations"]; present {
		t.Fatalf("empty capability observations were published: %s", raw)
	}
}

func TestLoadHostListRowsAttachesStoredCapabilityObservations(t *testing.T) {
	database := db.SetupTestDB(t)
	setTestHostInventory(t, []inventory.HostSpec{{
		Name:         "studio",
		Capabilities: []string{"agent:claude"},
	}})
	configDir := t.TempDir()
	restoreConfigPaths := config.SetConfigPathsForTesting(
		filepath.Join(configDir, "config.toml"),
		filepath.Join(configDir, "config.yaml"),
	)
	t.Cleanup(restoreConfigPaths)
	oldJSONFlag := hostListJSONFlag
	hostListJSONFlag = true
	t.Cleanup(func() { hostListJSONFlag = oldJSONFlag })

	if err := db.RecordHostCapabilityObservation(database, db.HostCapabilityObservation{
		Host:       "studio",
		Label:      "agent:claude",
		Source:     "probe:agent-review",
		Observed:   false,
		ObservedAt: time.Unix(1_787_700_000, 0),
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := loadHostListRows(time.Now(), rentalsOff)
	if err != nil {
		t.Fatalf("loadHostListRows: %v", err)
	}
	if len(rows) != 1 || len(rows[0].CapabilityObservations) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestHostListJSONOmitsDisplayFormattedColumns(t *testing.T) {
	// Rentals carry an em dash for CPU and memory. Publishing the table's
	// strings would make those placeholders part of the contract.
	rows := []hostListRow{{
		Type: "rental", Name: "wi123",
		OSArch: "vastai", CPU: "—", Memory: "—", GPUs: "unknown",
	}}
	_, raw := decodeHostListJSON(t, rows)

	for _, forbidden := range []string{"—", "unknown", "os_arch", "cpu", "memory", "gpus"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("JSON contains display artifact %q: %s", forbidden, raw)
		}
	}
}

func TestHostListJSONLeavesRentalsWithoutConfiguredFields(t *testing.T) {
	testHostConfig := configureTestHosts(t, map[string]config.HostConfig{
		"wi123": {SSHUser: "root", Capabilities: []string{"agent:codex"}},
	})

	// A rental is not a configured host even when a config entry shares its
	// name; it is reached with per-launch credentials.
	rows := []hostListRow{{Type: "rental", Name: "wi123"}}
	applyHostConfigFields(rows, testHostConfig)
	doc, raw := decodeHostListJSON(t, rows)

	hosts, _ := doc["hosts"].([]any)
	host, _ := hosts[0].(map[string]any)
	for _, key := range []string{"ssh_target", "ssh_identity_file", "capabilities"} {
		if _, present := host[key]; present {
			t.Errorf("rental row carries %q: %s", key, raw)
		}
	}
}

func TestHostListJSONOmitsUnconfiguredHostFieldsRatherThanEmptyValues(t *testing.T) {
	testHostConfig := configureTestHosts(t, map[string]config.HostConfig{})

	// Absent must be distinguishable from declared-and-empty: a host weft has
	// no config for is missing evidence, not a host that declares nothing.
	rows := []hostListRow{{Type: "host", Name: "host-beta"}}
	applyHostConfigFields(rows, testHostConfig)
	doc, raw := decodeHostListJSON(t, rows)

	hosts, _ := doc["hosts"].([]any)
	host, _ := hosts[0].(map[string]any)
	if _, present := host["capabilities"]; present {
		t.Errorf("unconfigured host carries capabilities: %s", raw)
	}
	if host["name"] != "host-beta" || host["type"] != "host" {
		t.Errorf("identity fields dropped: %s", raw)
	}
}

// TestLoadHostListRowsKeepsDisprovedCapabilityAdvisory exercises the assembly
// path, where declared capabilities and observations are joined. The projection
// test above builds rows by hand and so cannot see filtering introduced here —
// and this is where a future change would most naturally add it. Advisory-only
// is the decision in docs/decisions/0021; it needs a test on the path that
// could break it.
func TestLoadHostListRowsKeepsDisprovedCapabilityAdvisory(t *testing.T) {
	database := db.SetupTestDB(t)
	setTestHostInventory(t, []inventory.HostSpec{{
		Name: "studio", OS: "darwin", Arch: "arm64", CPUCores: 12,
		Capabilities: []string{"agent:claude", "agent:codex"},
	}})
	configureTestHosts(t, map[string]config.HostConfig{
		"studio": {Capabilities: []string{"agent:claude", "agent:codex"}},
	})
	t.Cleanup(config.SetConfigPathsForTesting(
		filepath.Join(t.TempDir(), "config.toml"), filepath.Join(t.TempDir(), "config.yaml")))

	if err := db.RecordHostCapabilityObservation(database, db.HostCapabilityObservation{
		Host: "studio", Label: "agent:claude", Source: "probe:agent-review",
		Observed: false, Detail: "loggedIn=false", ObservedAt: time.Unix(1_787_700_000, 0),
	}); err != nil {
		t.Fatalf("record observation: %v", err)
	}

	hostListJSONFlag = true
	t.Cleanup(func() { hostListJSONFlag = false })

	rows, err := loadHostListRows(time.Unix(1_787_700_100, 0), rentalsOff)
	if err != nil {
		t.Fatalf("loadHostListRows: %v", err)
	}
	var studio *hostListRow
	for i := range rows {
		if rows[i].Name == "studio" {
			studio = &rows[i]
		}
	}
	if studio == nil {
		t.Fatalf("studio row missing from %d rows", len(rows))
	}
	if !slices.Contains(studio.Capabilities, "agent:claude") {
		t.Errorf("a disproved observation removed agent:claude from the declared set: %v", studio.Capabilities)
	}
	if len(studio.CapabilityObservations) != 1 {
		t.Errorf("observations = %d, want 1", len(studio.CapabilityObservations))
	}
}
