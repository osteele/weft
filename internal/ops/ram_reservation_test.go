package ops

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestJobRAMReservationKBUsesLargestSignal(t *testing.T) {
	declaredGB := 30
	predictedKB := float64(40 * 1024 * 1024)
	job := &db.Job{
		CLIResourceOverrides: &db.CLIResourceOverrides{CPUMemGB: &declaredGB},
		PlacementMeta:        &db.PlacementMeta{PredictedRSSUpperKB: &predictedKB},
	}
	if got, want := jobRAMReservationKB(job), int64(40*1024*1024); got != want {
		t.Fatalf("jobRAMReservationKB = %d, want %d", got, want)
	}

	predictedKB = float64(16 * 1024 * 1024)
	if got, want := jobRAMReservationKB(job), int64(32*1024*1024); got != want {
		t.Fatalf("jobRAMReservationKB = %d, want declared 30 GiB + 2 GiB headroom = %d", got, want)
	}
}
