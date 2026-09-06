package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/spf13/cobra"
)

// TestServedMirrorSectionsHaveProducers kills a mutation that advertises a
// static mirror command without registering its hub-side section producer.
func TestServedMirrorSectionsHaveProducers(t *testing.T) {
	for command, section := range edgeMirrorServed {
		if section == edgeMirrorJobDetail || section == edgeMirrorJobLogTail {
			continue
		}
		if edgeViewStaticProducers[section] == nil {
			t.Errorf("served command %q has no producer for %q", command, section)
		}
	}
}

// TestEdgeViewProducersCarryIndexJobs kills a mutation that publishes only
// the index and omits the job-scoped detail or log-tail keys.
func TestEdgeViewProducersCarryIndexJobs(t *testing.T) {
	withoutUsageHints(t)
	_, deps := seedJobDetailFixture(t)
	producers := edgeViewProducers(context.Background(), deps)
	for _, section := range []string{edgeview.JobDetailSection("wj42"), edgeview.JobLogTailSection("wj42")} {
		if producers[section] == nil {
			t.Errorf("no producer for %s", section)
		}
	}
}

// TestShouldPublishEdgeViewPinsRoleAndConfiguration kills mutations that
// start the publisher on an edge or on a hub without a view store.
func TestShouldPublishEdgeViewPinsRoleAndConfiguration(t *testing.T) {
	configured := config.EdgeViewConfig{Bucket: "view"}
	for name, tc := range map[string]struct {
		cfg  *config.Config
		want bool
	}{
		"configured hub": {cfg: &config.Config{Edge: config.EdgeConfig{Role: "hub", View: configured}}, want: true},
		"default hub":    {cfg: &config.Config{}, want: false},
		"configured edge": {cfg: &config.Config{Edge: config.EdgeConfig{
			Role: "edge", View: configured,
		}}, want: false},
		"nil": {cfg: nil, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := shouldPublishEdgeView(tc.cfg); got != tc.want {
				t.Fatalf("shouldPublishEdgeView = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestEdgeViewNextWakePinsDebounceAndHeartbeat kills mutations that publish
// inside five seconds or delay a heartbeat behind a later debounce deadline.
func TestEdgeViewNextWakePinsDebounceAndHeartbeat(t *testing.T) {
	last := time.Date(2026, 9, 8, 1, 0, 0, 0, time.UTC)
	if got, want := edgeViewNextWake(last, last.Add(time.Minute), true), last.Add(5*time.Second); got != want {
		t.Fatalf("pending change wake = %s, want %s", got, want)
	}
	if got, want := edgeViewNextWake(last, last.Add(3*time.Second), true), last.Add(3*time.Second); got != want {
		t.Fatalf("heartbeat-first wake = %s, want %s", got, want)
	}
	if got, want := edgeViewNextWake(last, last.Add(time.Minute), false), last.Add(time.Minute); got != want {
		t.Fatalf("quiet wake = %s, want %s", got, want)
	}
}

// TestEdgeViewPublishStateRoundTrip kills a mutation that stores publisher
// health in the jobs database or drops per-section failure details.
func TestEdgeViewPublishStateRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	want := edgeViewPublishState{
		LastAttempt:           time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC),
		LastSuccessfulPublish: time.Date(2026, 9, 8, 1, 2, 0, 0, time.UTC),
		SectionErrors:         map[string]string{"hosts.json": "inventory unavailable"},
		PublishError:          "store timeout",
	}
	if err := saveEdgeViewPublishState(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadEdgeViewPublishState()
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastAttempt.Equal(want.LastAttempt) || !got.LastSuccessfulPublish.Equal(want.LastSuccessfulPublish) ||
		got.SectionErrors["hosts.json"] != "inventory unavailable" || got.PublishError != "store timeout" {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

// TestEdgeDoctorReportsManifestAndSectionFreshness kills a mutation that
// reports only store reachability without the manifest and section ages.
func TestEdgeDoctorReportsManifestAndSectionFreshness(t *testing.T) {
	root := filepath.Join(t.TempDir(), "view")
	transport, err := edge.NewFSTransport(root)
	if err != nil {
		t.Fatal(err)
	}
	pub := edgeview.NewPublisher(transport, "hub-a", "test")
	if _, err := pub.Publish(context.Background(), map[string]edgeview.Producer{
		edgeview.SectionJobsIndex: func(context.Context) ([]byte, error) { return []byte(`{"jobs":[]}`), nil },
		edgeview.SectionHosts:     func(context.Context) ([]byte, error) { return nil, errors.New("inventory unavailable") },
	}); err != nil {
		t.Fatal(err)
	}
	minutes := 5
	cfg := &config.Config{Edge: config.EdgeConfig{Role: "edge", View: config.EdgeViewConfig{StaleAfterMinutes: &minutes}}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	printEdgeViewDoctor(cmd, cfg, "edge", root)
	for _, want := range []string{
		"View manifest: present, age 0s (bound 5m, fresh)",
		"View section jobs/index.json: age 0s (bound 5m, fresh)",
		"View section hosts.json: FAILED: inventory unavailable",
	} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Errorf("doctor output %q lacks %q", out.String(), want)
		}
	}
}

// TestHubDoctorReportsPersistedPublisherState kills a mutation that consults
// the jobs ledger or omits failures from the last publication.
func TestHubDoctorReportsPersistedPublisherState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	state := edgeViewPublishState{
		LastAttempt:           time.Now(),
		LastSuccessfulPublish: time.Now().Add(-time.Minute),
		SectionErrors:         map[string]string{"jobs/wj42/log.tail": "snapshot unavailable"},
	}
	if err := saveEdgeViewPublishState(state); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Edge: config.EdgeConfig{View: config.EdgeViewConfig{Bucket: "view"}}}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printEdgeViewDoctor(cmd, cfg, "hub", "")
	if !bytes.Contains(out.Bytes(), []byte("View:      last published 1m ago")) ||
		!bytes.Contains(out.Bytes(), []byte("View section jobs/wj42/log.tail: FAILED: snapshot unavailable")) {
		t.Fatalf("hub doctor output = %q", out.String())
	}
}
