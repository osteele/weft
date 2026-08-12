package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func TestCollectWeftBugReclassifyTargets(t *testing.T) {
	database := db.SetupTestDB(t)
	id := makeReclassifyFixture(t, database, db.LaunchStatusFailed,
		db.TerminationReasonInfraFailure, "false watchdog teardown", time.Now())

	got, err := collectWeftBugReclassifyTargets(database, []string{ids.FormatInstanceID(id)})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 1 || got[0] != id {
		t.Fatalf("targets = %v, want [%d]", got, id)
	}
}

func TestCollectWeftBugReclassifyTargetsRejectsSpecificReason(t *testing.T) {
	database := db.SetupTestDB(t)
	id := makeReclassifyFixture(t, database, db.LaunchStatusFailed,
		db.TerminationReasonDiskFull, "disk full", time.Now())

	_, err := collectWeftBugReclassifyTargets(database, []string{ids.FormatInstanceID(id)})
	if err == nil || !strings.Contains(err.Error(), "not eligible") {
		t.Fatalf("error = %v, want ineligible-reason error", err)
	}
}

func TestReclassifyWeftBugPreservesEvidence(t *testing.T) {
	database := db.SetupTestDB(t)
	id := makeReclassifyFixture(t, database, db.LaunchStatusFailed,
		db.TerminationReasonInfraFailure, "false watchdog teardown", time.Now())

	if err := db.ReclassifyLaunchTerminationReason(database, id, db.TerminationReasonWeftBug,
		"confirmed Weft defect"); err != nil {
		t.Fatalf("reclassify: %v", err)
	}
	launch, err := db.GetLaunch(database, id)
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if launch.TerminationReason != db.TerminationReasonWeftBug {
		t.Fatalf("termination reason = %q, want %q", launch.TerminationReason, db.TerminationReasonWeftBug)
	}
	if !strings.Contains(launch.TerminationDetail, "confirmed Weft defect") ||
		!strings.Contains(launch.TerminationDetail, "false watchdog teardown") {
		t.Fatalf("termination detail did not preserve evidence: %q", launch.TerminationDetail)
	}
}
