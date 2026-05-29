package terminal

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestRequestWatchJobCordon_CloudInstance(t *testing.T) {
	database := db.SetupTestDB(t)
	defer database.Close()

	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	job := &db.Job{ID: 1, LaunchID: &launchID}

	msg, ok := requestWatchJobCordon(database, job, true)().(watchCordonDoneMsg)
	if !ok {
		t.Fatalf("requestWatchJobCordon returned %T, want watchCordonDoneMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("cordon failed: %v", msg.err)
	}
	if !msg.cordoned {
		t.Errorf("cordoned = false, want true")
	}
	if !strings.HasPrefix(msg.targetLabel, "instance ") {
		t.Errorf("targetLabel = %q, want it to start with %q", msg.targetLabel, "instance ")
	}

	got, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if !got.Cordoned {
		t.Errorf("launch.Cordoned = false after cordon")
	}

	uncordonMsg, ok := requestWatchJobCordon(database, job, false)().(watchCordonDoneMsg)
	if !ok {
		t.Fatalf("uncordon returned %T, want watchCordonDoneMsg", uncordonMsg)
	}
	if uncordonMsg.err != nil {
		t.Fatalf("uncordon failed: %v", uncordonMsg.err)
	}
	if uncordonMsg.cordoned {
		t.Errorf("cordoned = true after uncordon, want false")
	}
	got, err = db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if got.Cordoned {
		t.Errorf("launch.Cordoned = true after uncordon")
	}
}

func TestRequestWatchJobCordon_InventoryHost(t *testing.T) {
	database := db.SetupTestDB(t)
	defer database.Close()

	job := &db.Job{ID: 1, Host: "cool30"}

	msg, ok := requestWatchJobCordon(database, job, true)().(watchCordonDoneMsg)
	if !ok {
		t.Fatalf("requestWatchJobCordon returned %T, want watchCordonDoneMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("cordon failed: %v", msg.err)
	}
	if msg.targetLabel != "host cool30" {
		t.Errorf("targetLabel = %q, want %q", msg.targetLabel, "host cool30")
	}

	cordoned, _, err := db.IsInventoryExecutionTargetCordoned(database, "cool30")
	if err != nil {
		t.Fatalf("IsInventoryExecutionTargetCordoned: %v", err)
	}
	if !cordoned {
		t.Errorf("host cool30 not cordoned after toggle")
	}
}

func TestRequestWatchJobCordon_UnplacedJob(t *testing.T) {
	database := db.SetupTestDB(t)
	defer database.Close()

	job := &db.Job{ID: 1} // no LaunchID, no Host
	msg, ok := requestWatchJobCordon(database, job, true)().(watchCordonDoneMsg)
	if !ok {
		t.Fatalf("requestWatchJobCordon returned %T, want watchCordonDoneMsg", msg)
	}
	if msg.err == nil {
		t.Errorf("expected error for unplaced job, got nil (label=%q)", msg.targetLabel)
	}
}
