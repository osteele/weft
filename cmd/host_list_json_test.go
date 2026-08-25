package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/config"
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
			Capabilities:    []string{"agent:codex", "agent:claude"},
		},
	})

	rows := []hostListRow{{Type: "host", Name: "host-alpha"}}
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
