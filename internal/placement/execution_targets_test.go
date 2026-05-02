package placement

import (
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
)

func TestScoreHostsSkipsCordonedInventoryTarget(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.SetInventoryExecutionTargetCordoned(database, "host-alpha", true, "interactive session"); err != nil {
		t.Fatalf("SetInventoryExecutionTargetCordoned: %v", err)
	}

	hosts := []inventory.HostSpec{
		{Name: "host-alpha", GPUs: []inventory.GPUSpec{{Name: "A100", Class: "a100", Memory: "80GB"}}},
		{Name: "host-beta", GPUs: []inventory.GPUSpec{{Name: "A100", Class: "a100", Memory: "80GB"}}},
	}
	scores := ScoreHostListWithMetrics(database, hosts, Constraints{GPUClass: "a100"}, nil)

	for _, score := range scores {
		switch score.Host {
		case "host-alpha":
			if score.Eligible {
				t.Fatalf("cordoned host-alpha is eligible: %#v", score)
			}
		case "host-beta":
			if !score.Eligible {
				t.Fatalf("uncordoned host-beta is not eligible: %#v", score)
			}
		}
	}
}
