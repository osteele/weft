package bidding

import (
	"strings"
	"testing"
	"time"

	jobdb "github.com/osteele/weft/internal/db"
)

func TestCheckOfferConcentration_NarrowConstraintWarns(t *testing.T) {
	database := jobdb.SetupTestDB(t)

	now := time.Now().Unix()
	insertLaunch := func(vram int, dc string) {
		_, err := database.Exec(`
			INSERT INTO launches (status, provider, gpu_spec, gpu_class, gpu_mem_gb,
				resolved_gpu_name, termination_reason, ended_at, data_center,
				provider_instance_id, created_at)
			VALUES ('failed', 'vastai', ?, '4090', ?, 'RTX 4090', 'unknown', ?, ?, '0', ?)
		`, "spec", vram, now, dc, now)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	for range 50 {
		insertLaunch(24, "Texas, US")
	}
	for range 5 {
		insertLaunch(48, "Sichuan, CN")
	}

	w, err := CheckOfferConcentration(database, "4090", 26)
	if err != nil {
		t.Fatalf("CheckOfferConcentration: %v", err)
	}
	if w.Message == "" {
		t.Fatal("expected a concentration warning for 26 GB request, got none")
	}
	if w.Candidates != 5 || w.FamilyTotal != 55 {
		t.Errorf("counts: candidates=%d total=%d, want 5/55", w.Candidates, w.FamilyTotal)
	}
	if w.TopRegion != "Sichuan, CN" {
		t.Errorf("expected top region Sichuan, CN; got %q", w.TopRegion)
	}
	if !strings.Contains(w.Message, "Sichuan, CN") {
		t.Errorf("message should name the dominant region; got %q", w.Message)
	}
}

func TestCheckOfferConcentration_HealthyConstraintQuiet(t *testing.T) {
	database := jobdb.SetupTestDB(t)
	now := time.Now().Unix()
	for range 50 {
		_, err := database.Exec(`
			INSERT INTO launches (status, provider, gpu_spec, gpu_class, gpu_mem_gb,
				resolved_gpu_name, termination_reason, ended_at, data_center,
				provider_instance_id, created_at)
			VALUES ('completed', 'vastai', 'spec', '4090', 24, 'RTX 4090', 'completed', ?, 'Texas, US', '0', ?)
		`, now, now)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	w, err := CheckOfferConcentration(database, "4090", 22)
	if err != nil {
		t.Fatalf("CheckOfferConcentration: %v", err)
	}
	if w.Message != "" {
		t.Errorf("expected no warning for healthy 22 GB request; got %q", w.Message)
	}
}

func TestCheckOfferConcentration_ColdStartQuiet(t *testing.T) {
	database := jobdb.SetupTestDB(t)
	w, err := CheckOfferConcentration(database, "5090", 32)
	if err != nil {
		t.Fatalf("CheckOfferConcentration: %v", err)
	}
	if w.Message != "" {
		t.Errorf("expected silence on cold start; got %q", w.Message)
	}
}
