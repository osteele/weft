package db

import (
	"math"
	"testing"
)

func TestListProjectSpendSince_Proration(t *testing.T) {
	database := SetupTestDB(t)

	insertSpendRun := func(id int64, project string, start, end int64, cost float64) {
		t.Helper()
		insertTestJob(t, database, id, "python train.py", "/tmp/"+project, StatusCompleted, withStartTime(start), withEndTime(end))
		if _, err := database.Exec(`UPDATE jobs SET project = ? WHERE id = ?`, project, id); err != nil {
			t.Fatalf("set project: %v", err)
		}
		if _, err := database.Exec(`UPDATE job_attempts SET cost = ? WHERE job_id = ?`, cost, id); err != nil {
			t.Fatalf("set attempt cost: %v", err)
		}
	}

	insertSpendRun(101, "alpha", 100, 200, 10.0) // 50% overlap after 150 => $5.00
	insertSpendRun(102, "alpha", 160, 220, 8.0)  // full overlap => $8.00
	insertSpendRun(103, "beta", 140, 155, 4.0)   // 5/15 overlap => $1.333...
	insertSpendRun(104, "beta", 120, 140, 9.0)   // no overlap => $0

	rows, err := ListProjectSpendSince(database, 150)
	if err != nil {
		t.Fatalf("ListProjectSpendSince: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows len = %d, want 2", len(rows))
	}

	if rows[0].Project != "alpha" {
		t.Fatalf("rows[0].Project = %q, want %q", rows[0].Project, "alpha")
	}
	if math.Abs(rows[0].SpentUSD-13.0) > 1e-9 {
		t.Fatalf("alpha spent = %.6f, want 13.0", rows[0].SpentUSD)
	}
	if rows[0].RunCount != 2 {
		t.Fatalf("alpha run count = %d, want 2", rows[0].RunCount)
	}

	if rows[1].Project != "beta" {
		t.Fatalf("rows[1].Project = %q, want %q", rows[1].Project, "beta")
	}
	if math.Abs(rows[1].SpentUSD-(4.0/3.0)) > 1e-9 {
		t.Fatalf("beta spent = %.6f, want %.6f", rows[1].SpentUSD, 4.0/3.0)
	}
	if rows[1].RunCount != 1 {
		t.Fatalf("beta run count = %d, want 1", rows[1].RunCount)
	}
}

func TestListProjectSpendSince_EmptyProjectName(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 201, "python train.py", "/tmp/x", StatusCompleted, withStartTime(200), withEndTime(300))
	if _, err := database.Exec(`UPDATE jobs SET project = '' WHERE id = ?`, 201); err != nil {
		t.Fatalf("set project blank: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET cost = ? WHERE job_id = ?`, 6.5, 201); err != nil {
		t.Fatalf("set attempt cost: %v", err)
	}

	rows, err := ListProjectSpendSince(database, 150)
	if err != nil {
		t.Fatalf("ListProjectSpendSince: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0].Project != "(none)" {
		t.Fatalf("project = %q, want %q", rows[0].Project, "(none)")
	}
	if math.Abs(rows[0].SpentUSD-6.5) > 1e-9 {
		t.Fatalf("spent = %.6f, want 6.5", rows[0].SpentUSD)
	}
}
