package cmd

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/edge"
)

func writeRenewalTestKey(t *testing.T, dir string, key edge.Key) {
	t.Helper()
	key.PublicKey = base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	if err := edge.SaveKey(dir, key); err != nil {
		t.Fatal(err)
	}
}

func renewalStatusRunner(t *testing.T, projects []researchSiteProject, runErr error) researchSiteStatusRunner {
	t.Helper()
	if projects == nil {
		projects = []researchSiteProject{}
	}
	data, err := json.Marshal(researchSiteStatus{
		SchemaVersion: researchSiteStatusSchema,
		Projects:      projects,
	})
	if err != nil {
		t.Fatal(err)
	}
	return func(context.Context) ([]byte, string, error) {
		return data, "one project unavailable", runErr
	}
}

func TestRenewEdgeKeysOnceRenewsSettledRemoteProjectOnKeyHost(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	lease := edge.DefaultLeaseConfig()
	dir := t.TempDir()
	writeRenewalTestKey(t, dir, edge.Key{
		KeyID: "studio-plan-7", Host: "studio", PlanID: "plan-7", Project: "lm2",
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
	})

	report := renewEdgeKeysOnce(context.Background(), dir, lease, now,
		renewalStatusRunner(t, []researchSiteProject{{
			Project: "lm2", OwnedBy: "remote", Settled: true, RemoteHost: "agent@studio",
		}}, nil))
	if len(report.Problems) != 0 || len(report.Deferred) != 0 {
		t.Fatalf("report = %+v, want an unqualified renewal", report)
	}
	if len(report.Renewed) != 1 || report.Renewed[0] != "studio-plan-7" {
		t.Fatalf("renewed = %v", report.Renewed)
	}
	ring, err := edge.LoadKeyring(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := ring.Lookup("studio-plan-7")
	if !ok || !key.NotAfter.Equal(now.Add(lease.Window)) {
		t.Fatalf("persisted deadline = %s, want %s", key.NotAfter, now.Add(lease.Window))
	}
}

func TestRenewEdgeKeyOnceRequiresSettledRemoteOwnership(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	lease := edge.DefaultLeaseConfig()
	dir := t.TempDir()
	originalDeadline := now.Add(5 * time.Hour)
	writeRenewalTestKey(t, dir, edge.Key{
		KeyID: "studio-plan-7", Host: "studio", PlanID: "plan-7", Project: "lm2",
		NotBefore: now.Add(-time.Hour), NotAfter: originalDeadline,
	})
	ring, err := edge.LoadKeyring(dir)
	if err != nil {
		t.Fatal(err)
	}

	report := renewEdgeKeyOnce(context.Background(), ring, "studio-plan-7", lease, now,
		renewalStatusRunner(t, []researchSiteProject{{
			Project: "lm2", OwnedBy: "local", Settled: true, RemoteHost: "studio",
		}}, nil))
	if len(report.Renewed) != 0 || len(report.Deferred) != 1 {
		t.Fatalf("manual renewal bypassed ownership: %+v", report)
	}
	key, _ := ring.Lookup("studio-plan-7")
	if !key.NotAfter.Equal(originalDeadline) {
		t.Fatalf("deadline moved without remote ownership: %s", key.NotAfter)
	}

	report = renewEdgeKeyOnce(context.Background(), ring, "studio-plan-7", lease, now,
		renewalStatusRunner(t, []researchSiteProject{{
			Project: "lm2", OwnedBy: "remote", Settled: true, RemoteHost: "agent@studio",
		}}, nil))
	if len(report.Renewed) != 1 || report.Renewed[0] != "studio-plan-7" {
		t.Fatalf("settled remote ownership did not renew exact key: %+v", report)
	}
	key, _ = ring.Lookup("studio-plan-7")
	if !key.NotAfter.Equal(now.Add(lease.Window)) {
		t.Fatalf("renewed deadline = %s, want %s", key.NotAfter, now.Add(lease.Window))
	}
}

func TestRenewEdgeKeysOnceRequiresUnambiguousSettledRemoteOwnership(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	lease := edge.DefaultLeaseConfig()
	tests := []struct {
		name     string
		projects []researchSiteProject
		contains string
	}{
		{name: "missing", contains: "absent"},
		{name: "duplicate", projects: []researchSiteProject{
			{Project: "lm2", OwnedBy: "remote", Settled: true, RemoteHost: "studio"},
			{Project: "lm2", OwnedBy: "remote", Settled: true, RemoteHost: "studio"},
		}, contains: "ambiguous"},
		{name: "handoff", projects: []researchSiteProject{{Project: "lm2", OwnedBy: "remote", Settled: false, RemoteHost: "studio"}}, contains: "handoff in flight"},
		{name: "local", projects: []researchSiteProject{{Project: "lm2", OwnedBy: "local", Settled: true, RemoteHost: "studio"}}, contains: "not remote"},
		{name: "wrong host", projects: []researchSiteProject{{Project: "lm2", OwnedBy: "remote", Settled: true, RemoteHost: "agent@cool30"}}, contains: "not key host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			deadline := now.Add(time.Hour)
			writeRenewalTestKey(t, dir, edge.Key{
				KeyID: "studio-plan-7", Host: "studio", Project: "lm2",
				NotBefore: now.Add(-time.Hour), NotAfter: deadline,
			})
			report := renewEdgeKeysOnce(context.Background(), dir, lease, now,
				renewalStatusRunner(t, tc.projects, nil))
			if len(report.Renewed) != 0 {
				t.Fatalf("renewed without authoritative ownership: %+v", report)
			}
			all := strings.Join(append(append([]string{}, report.Problems...), report.Deferred...), "\n")
			if !strings.Contains(all, tc.contains) {
				t.Fatalf("report = %+v, want %q", report, tc.contains)
			}
			ring, err := edge.LoadKeyring(dir)
			if err != nil {
				t.Fatal(err)
			}
			key, _ := ring.Lookup("studio-plan-7")
			if !key.NotAfter.Equal(deadline) {
				t.Fatalf("deadline moved from %s to %s", deadline, key.NotAfter)
			}
		})
	}
}

func TestRenewEdgeKeysOnceUsesValidRowsFromPartialStatusFailure(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writeRenewalTestKey(t, dir, edge.Key{
		KeyID: "studio-plan-7", Host: "studio", Project: "lm2",
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
	})
	report := renewEdgeKeysOnce(context.Background(), dir, edge.DefaultLeaseConfig(), now,
		renewalStatusRunner(t, []researchSiteProject{{
			Project: "lm2", OwnedBy: "remote", Settled: true, RemoteHost: "studio",
		}}, errors.New("exit status 1")))
	if len(report.Renewed) != 1 {
		t.Fatalf("healthy row was discarded with partial source failure: %+v", report)
	}
	if len(report.Problems) != 1 || !strings.Contains(report.Problems[0], "one project unavailable") {
		t.Fatalf("partial failure was hidden: %+v", report)
	}
}

func TestRenewEdgeKeysOnceRejectsUnknownStatusSchema(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	deadline := now.Add(time.Hour)
	writeRenewalTestKey(t, dir, edge.Key{
		KeyID: "studio-plan-7", Host: "studio", Project: "lm2",
		NotBefore: now.Add(-time.Hour), NotAfter: deadline,
	})
	report := renewEdgeKeysOnce(context.Background(), dir, edge.DefaultLeaseConfig(), now,
		func(context.Context) ([]byte, string, error) {
			return []byte(`{"schema_version":"research-site.status/v2","projects":[]}`), "", nil
		})
	if len(report.Renewed) != 0 || len(report.Problems) != 1 || !strings.Contains(report.Problems[0], "schema") {
		t.Fatalf("report = %+v", report)
	}
}

func TestRenewEdgeKeysOnceNeverReopensLapsedKey(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	deadline := now.Add(-time.Minute)
	writeRenewalTestKey(t, dir, edge.Key{
		KeyID: "studio-plan-7", Host: "studio", Project: "lm2",
		NotBefore: now.Add(-time.Hour), NotAfter: deadline,
	})
	called := false
	report := renewEdgeKeysOnce(context.Background(), dir, edge.DefaultLeaseConfig(), now,
		func(context.Context) ([]byte, string, error) {
			called = true
			return nil, "", nil
		})
	if called || len(report.Renewed) != 0 {
		t.Fatalf("lapsed key reached renewal source: called=%v report=%+v", called, report)
	}
	ring, err := edge.LoadKeyring(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ring.Lookup("studio-plan-7")
	if !key.NotAfter.Equal(deadline) {
		t.Fatalf("lapsed deadline moved from %s to %s", deadline, key.NotAfter)
	}
}

func TestRenewEdgeKeysOnceDoesNotRewriteFreshKey(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	lease := edge.DefaultLeaseConfig()
	dir := t.TempDir()
	deadline := now.Add(lease.Window)
	writeRenewalTestKey(t, dir, edge.Key{
		KeyID: "studio-plan-7", Host: "studio", Project: "lm2",
		NotBefore: now.Add(-time.Hour), NotAfter: deadline,
	})
	called := false
	report := renewEdgeKeysOnce(context.Background(), dir, lease, now,
		func(context.Context) ([]byte, string, error) {
			called = true
			return nil, "", nil
		})
	if called || len(report.Renewed) != 0 || len(report.Problems) != 0 {
		t.Fatalf("fresh key caused work: called=%v report=%+v", called, report)
	}
}

func TestEdgeLeaseConfigAppliesRenewalInterval(t *testing.T) {
	cfg := &config.Config{}
	cfg.Edge.LeaseWindowHours = 8
	cfg.Edge.LeaseRenewIntervalHours = 2
	lease, err := edgeLeaseConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Window != 8*time.Hour || lease.Interval != 2*time.Hour {
		t.Fatalf("lease = %+v", lease)
	}

	cfg.Edge.LeaseRenewIntervalHours = 8
	if _, err := edgeLeaseConfig(cfg); err == nil {
		t.Fatal("accepted renewal interval equal to window")
	}
}

func TestShouldRenewEdgeKeysOnlyOnHub(t *testing.T) {
	if !shouldRenewEdgeKeys(&config.Config{}) {
		t.Fatal("hub did not start edge key renewer")
	}
	cfg := &config.Config{}
	cfg.Edge.Role = "edge"
	if shouldRenewEdgeKeys(cfg) {
		t.Fatal("edge started hub-side key renewer")
	}
}
