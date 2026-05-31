package cmd

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func TestCollectMarkCreditExhaustedTargets_ExplicitIDs(t *testing.T) {
	database := db.SetupTestDB(t)
	failedID := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, "", time.Now().Add(-30*time.Minute))

	plan, err := collectMarkCreditExhaustedTargets(database, []string{ids.FormatInstanceID(failedID)}, 0, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(plan.targets) != 1 || plan.targets[0].ID != failedID {
		t.Fatalf("targets = %+v, want [%d]", plan.targets, failedID)
	}
	if len(plan.rejected) != 0 {
		t.Fatalf("rejected = %d, want 0 (explicit IDs do not appear as rejections)", len(plan.rejected))
	}
}

func TestCollectMarkCreditExhaustedTargets_SinceWindow_IncludeGeneric(t *testing.T) {
	database := db.SetupTestDB(t)
	recent := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, "", time.Now().Add(-10*time.Minute))
	old := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, "", time.Now().Add(-48*time.Hour))
	ineligible := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonDiskFull, "", time.Now().Add(-10*time.Minute))

	plan, err := collectMarkCreditExhaustedTargets(database, nil, 6*time.Hour, true)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	gotIDs := map[int64]bool{}
	for _, l := range plan.targets {
		gotIDs[l.ID] = true
	}
	if !gotIDs[recent] {
		t.Errorf("recent eligible launch %d missing", recent)
	}
	if gotIDs[old] {
		t.Errorf("old launch %d outside window should be filtered out", old)
	}
	if gotIDs[ineligible] {
		t.Errorf("ineligible (disk_full) launch %d should be filtered out", ineligible)
	}
}

func TestCollectMarkCreditExhaustedTargets_UnionAndDedup(t *testing.T) {
	database := db.SetupTestDB(t)
	inWindow := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, "", time.Now().Add(-30*time.Minute))
	explicit := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, "", time.Now().Add(-72*time.Hour))

	plan, err := collectMarkCreditExhaustedTargets(database,
		[]string{ids.FormatInstanceID(inWindow), ids.FormatInstanceID(explicit)},
		1*time.Hour, true)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(plan.targets) != 2 {
		t.Fatalf("targets = %d, want 2 (union with dedup)", len(plan.targets))
	}
	gotIDs := map[int64]bool{}
	for _, l := range plan.targets {
		gotIDs[l.ID] = true
	}
	if !gotIDs[inWindow] || !gotIDs[explicit] {
		t.Fatalf("missing IDs: got %v, want both %d and %d", gotIDs, inWindow, explicit)
	}
}

func TestCollectMarkCreditExhaustedTargets_RefusesNonTerminalExplicit(t *testing.T) {
	database := db.SetupTestDB(t)
	runningID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	_, err = collectMarkCreditExhaustedTargets(database, []string{ids.FormatInstanceID(runningID)}, 0, false)
	if err == nil {
		t.Fatalf("expected error for running launch, got nil")
	}
	if !strings.Contains(err.Error(), "failed/canceled") {
		t.Fatalf("error = %v, want message about failed/canceled", err)
	}
}

func TestCollectMarkCreditExhaustedTargets_RefusesIneligibleExplicit(t *testing.T) {
	database := db.SetupTestDB(t)
	diskFullID := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonDiskFull, "", time.Now().Add(-30*time.Minute))

	_, err := collectMarkCreditExhaustedTargets(database, []string{ids.FormatInstanceID(diskFullID)}, 0, false)
	if err == nil {
		t.Fatalf("expected error for disk_full launch, got nil")
	}
	if !strings.Contains(err.Error(), "not eligible") {
		t.Fatalf("error = %v, want message about ineligibility", err)
	}
}

func TestCollectMarkCreditExhaustedTargets_NoMatches(t *testing.T) {
	database := db.SetupTestDB(t)
	plan, err := collectMarkCreditExhaustedTargets(database, nil, 1*time.Hour, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(plan.targets) != 0 {
		t.Fatalf("targets = %d, want 0", len(plan.targets))
	}
	if len(plan.rejected) != 0 {
		t.Fatalf("rejected = %d, want 0", len(plan.rejected))
	}
}

// Smart filter: --since picks up strong-signature rows automatically.
func TestCollectMarkCreditExhaustedTargets_SmartFilter_StrongOnly(t *testing.T) {
	database := db.SetupTestDB(t)
	strongDetail := "instance creation failed: provider returned empty response: provider rejected create"
	strong1 := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, strongDetail, time.Now().Add(-30*time.Minute))
	strong2 := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, strongDetail, time.Now().Add(-25*time.Minute))
	// Unrelated dud-detection row: no credit signature, should be rejected.
	dud := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonInfraFailure,
		"dud provider: 8m20s post-running with no agent activity — terminating", time.Now().Add(-40*time.Minute))

	plan, err := collectMarkCreditExhaustedTargets(database, nil, 1*time.Hour, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	gotIncluded := map[int64]bool{}
	for _, l := range plan.targets {
		gotIncluded[l.ID] = true
	}
	if !gotIncluded[strong1] || !gotIncluded[strong2] {
		t.Errorf("strong-signal rows missing from targets: got %v", gotIncluded)
	}
	if gotIncluded[dud] {
		t.Errorf("dud-detection row %d should be rejected, but was included", dud)
	}
	var dudFound bool
	for _, r := range plan.rejected {
		if r.launch.ID == dud {
			dudFound = true
			if !strings.Contains(r.reason, "no credit signature") {
				t.Errorf("dud row rejected with reason %q, want one mentioning no credit signature", r.reason)
			}
		}
	}
	if !dudFound {
		t.Errorf("dud row %d missing from rejections", dud)
	}
}

// Smart filter: cluster-signal rows are included when they cluster with strong signals.
func TestCollectMarkCreditExhaustedTargets_SmartFilter_ClusterInWindow(t *testing.T) {
	database := db.SetupTestDB(t)
	clusterDetail := "provider dead with no completion or intent marker; provider status=destroyed"
	strongDetail := "instance creation failed: provider returned empty response"
	strongTime := time.Now().Add(-30 * time.Minute)
	// Cluster row ends 3 min before the strong-signal time — well inside the 15-min window.
	inCluster := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure,
		clusterDetail, strongTime.Add(-3*time.Minute))
	// Cluster row 2h before — outside the window.
	outCluster := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure,
		clusterDetail, strongTime.Add(-2*time.Hour))
	strong := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonInfraFailure,
		strongDetail, strongTime)

	plan, err := collectMarkCreditExhaustedTargets(database, nil, 3*time.Hour, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	gotIncluded := map[int64]bool{}
	for _, l := range plan.targets {
		gotIncluded[l.ID] = true
	}
	if !gotIncluded[strong] {
		t.Errorf("strong row %d missing", strong)
	}
	if !gotIncluded[inCluster] {
		t.Errorf("in-cluster row %d should be included", inCluster)
	}
	if gotIncluded[outCluster] {
		t.Errorf("out-of-cluster row %d should be rejected, but was included", outCluster)
	}
	var outFound bool
	for _, r := range plan.rejected {
		if r.launch.ID == outCluster {
			outFound = true
			if !strings.Contains(r.reason, "outside credit-cluster window") {
				t.Errorf("out-of-cluster row rejected with reason %q, want one mentioning the window", r.reason)
			}
		}
	}
	if !outFound {
		t.Errorf("out-of-cluster row %d missing from rejections", outCluster)
	}
}

// Smart filter: cluster-signal rows with NO anchoring strong signal are always rejected.
func TestCollectMarkCreditExhaustedTargets_SmartFilter_ClusterWithoutAnchor(t *testing.T) {
	database := db.SetupTestDB(t)
	clusterDetail := "provider dead with no completion or intent marker; provider status=destroyed"
	orphan := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure,
		clusterDetail, time.Now().Add(-30*time.Minute))

	plan, err := collectMarkCreditExhaustedTargets(database, nil, 1*time.Hour, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(plan.targets) != 0 {
		t.Fatalf("targets = %d, want 0 (no strong anchor → no cluster)", len(plan.targets))
	}
	var found bool
	for _, r := range plan.rejected {
		if r.launch.ID == orphan {
			found = true
			if !strings.Contains(r.reason, "anchor") {
				t.Errorf("orphan cluster row rejected with reason %q, want one mentioning missing anchor", r.reason)
			}
		}
	}
	if !found {
		t.Errorf("orphan cluster row %d missing from rejections", orphan)
	}
}

// --include-generic restores the old broad behavior.
func TestCollectMarkCreditExhaustedTargets_IncludeGenericOverridesFilter(t *testing.T) {
	database := db.SetupTestDB(t)
	dud := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonInfraFailure,
		"dud provider: 8m20s post-running with no agent activity", time.Now().Add(-30*time.Minute))

	plan, err := collectMarkCreditExhaustedTargets(database, nil, 1*time.Hour, true)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var got bool
	for _, l := range plan.targets {
		if l.ID == dud {
			got = true
			break
		}
	}
	if !got {
		t.Errorf("dud row %d should be included under --include-generic", dud)
	}
	if len(plan.rejected) != 0 {
		t.Errorf("rejected = %d, want 0 under --include-generic", len(plan.rejected))
	}
}

func makeReclassifyFixture(t *testing.T, database *sql.DB, status, reason, detail string, ended time.Time) int64 {
	t.Helper()
	id, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusPlanned,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE launches SET status = ?, ended_at = ?, termination_reason = ?, termination_detail = ? WHERE id = ?`,
		status, ended.Unix(), reason, detail, id,
	); err != nil {
		t.Fatalf("update fixture: %v", err)
	}
	return id
}
