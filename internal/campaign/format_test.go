package campaign

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

func TestFormatResolvedGPU(t *testing.T) {
	tests := []struct {
		gpuSpec, resolvedName, want string
	}{
		{"L40S", "L40S", "L40S"},                              // exact match
		{"l40s", "L40S", "L40S"},                              // case-insensitive match
		{"HOPPER+", "H200 NVL", "HOPPER+ → H200 NVL"},         // constraint differs
		{"A100 ≥80GB", "A100 PCIE", "A100 ≥80GB → A100 PCIE"}, // constraint with mem
	}
	for _, tt := range tests {
		got := FormatResolvedGPU(tt.gpuSpec, tt.resolvedName)
		if got != tt.want {
			t.Errorf("FormatResolvedGPU(%q, %q) = %q, want %q", tt.gpuSpec, tt.resolvedName, got, tt.want)
		}
	}
}

func TestFormatJobLine(t *testing.T) {
	job := &db.Job{ID: 88, Description: "EXP-030: Idle power investigation"}
	line := FormatJobLine(job)
	if !strings.Contains(line, "88") {
		t.Errorf("FormatJobLine should contain job ID, got %q", line)
	}
	if !strings.Contains(line, "EXP-030") {
		t.Errorf("FormatJobLine should contain description, got %q", line)
	}
}

func TestFormatJobLine_NoDescription(t *testing.T) {
	job := &db.Job{ID: 42, Command: "python train.py --model large"}
	line := FormatJobLine(job)
	if !strings.Contains(line, "42") {
		t.Errorf("FormatJobLine should contain job ID, got %q", line)
	}
	if !strings.Contains(line, "python train.py") {
		t.Errorf("FormatJobLine should contain command, got %q", line)
	}
}

func TestFormatJobIDs(t *testing.T) {
	jobs := make([]*db.Job, 7)
	for i := range jobs {
		jobs[i] = &db.Job{ID: int64(88 + i)}
	}

	result := FormatJobIDs(jobs, 5)
	if !strings.Contains(result, "88") {
		t.Errorf("should contain first job ID, got %q", result)
	}
	if !strings.Contains(result, "…+2") {
		t.Errorf("should contain overflow count, got %q", result)
	}

	// Small list
	small := FormatJobIDs(jobs[:3], 5)
	if strings.Contains(small, "…") {
		t.Errorf("small list should not have overflow, got %q", small)
	}
}

func TestFormatCostTable(t *testing.T) {
	offers := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "A100", GPUMemGB: 80, Jobs: make([]*db.Job, 7)},
			Offer: &cloud.Offer{GPUName: "RTX_A6000", GPUMemGB: 48, CostPerHour: 0.45},
		},
		{
			Group: InstanceGroup{GPUClass: "H100", GPUMemGB: 80, Jobs: make([]*db.Job, 3)},
			Offer: nil, // no offer
		},
	}

	table := FormatCostTable(offers, []int{7, 3})
	text := costTableText(table)
	if !strings.Contains(text, "A100") {
		t.Errorf("cost table should contain GPU class, got %q", text)
	}
	if !strings.Contains(text, "0.45") {
		t.Errorf("cost table should contain cost, got %q", text)
	}
	if !strings.Contains(text, "Total") {
		t.Errorf("cost table should contain total, got %q", text)
	}
}

func TestFormatCostTable_NoOffers(t *testing.T) {
	offers := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "A100", GPUMemGB: 80},
			Offer: nil,
		},
	}
	table := FormatCostTable(offers, []int{0})
	text := costTableText(table)
	if !strings.Contains(text, "No offers") {
		t.Errorf("should show no offers message, got %q", text)
	}
}

func TestFormatSSHCommand(t *testing.T) {
	inst := &cloud.Instance{SSHHost: "ssh6.vast.ai", SSHPort: 34567}
	cmd := FormatSSHCommand(inst)
	if !strings.Contains(cmd, "ssh -p 34567") {
		t.Errorf("SSH command should contain port, got %q", cmd)
	}
	if !strings.Contains(cmd, "root@ssh6.vast.ai") {
		t.Errorf("SSH command should contain host, got %q", cmd)
	}
	if !strings.Contains(cmd, "StrictHostKeyChecking=no") {
		t.Errorf("SSH command should disable host key checking, got %q", cmd)
	}
}

func TestTotalEstimatedCost(t *testing.T) {
	offers := []GroupOffer{
		{
			Group: InstanceGroup{Jobs: make([]*db.Job, 7)},
			Offer: &cloud.Offer{CostPerHour: 0.45},
		},
		{
			Group: InstanceGroup{Jobs: make([]*db.Job, 3)},
			Offer: &cloud.Offer{CostPerHour: 2.10},
		},
		{
			Group: InstanceGroup{Jobs: make([]*db.Job, 5)},
			Offer: nil, // no offer, should be skipped
		},
	}

	total := TotalEstimatedCost(offers)
	expected := 7*0.45 + 3*2.10 // 3.15 + 6.30 = 9.45
	if total < expected-0.01 || total > expected+0.01 {
		t.Errorf("TotalEstimatedCost = %f, want ~%f", total, expected)
	}
}

func TestFormatCostTableSelected(t *testing.T) {
	estimates := []CostEstimate{
		{
			Group: InstanceGroup{GPUClass: "A100", GPUMemGB: 80, Jobs: make([]*db.Job, 4)},
			Offer: GroupOffer{Offer: &cloud.Offer{GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 0.52}},
			Breakdown: estimate.Breakdown{
				Total: estimate.Estimate{Mean: 2 * time.Hour, Lower: 1 * time.Hour, Upper: 4 * time.Hour},
			},
			TotalCost: 1.04,
		},
		{
			Group: InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24, Jobs: make([]*db.Job, 5)},
			Offer: GroupOffer{Offer: &cloud.Offer{GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.24}},
			Breakdown: estimate.Breakdown{
				Total: estimate.Estimate{Mean: 5 * time.Hour, Lower: 2 * time.Hour, Upper: 10 * time.Hour},
			},
			TotalCost: 1.20,
		},
	}

	t.Run("all selected", func(t *testing.T) {
		table := FormatCostTableSelected(estimates, []int{4, 5}, 0, 0)
		text := costTableText(table)
		if !strings.Contains(text, "A100") {
			t.Errorf("should show GPU name, got %q", text)
		}
		if !strings.Contains(text, "Total") {
			t.Errorf("should contain total line, got %q", text)
		}
		for _, cl := range table.Lines {
			if cl.Dimmed {
				t.Errorf("should not dim any line when all selected, but %q is dimmed", cl.Text)
			}
		}
	})

	t.Run("partial selection omits unselected", func(t *testing.T) {
		table := FormatCostTableSelected(estimates, []int{2, 0}, 0, 0)
		text := costTableText(table)
		// Group 2 has 0 selected — should not appear in output
		if strings.Contains(text, "0 jobs") {
			t.Error("deselected group should be omitted, not shown with 0 jobs")
		}
	})

	t.Run("proportional cost scaling", func(t *testing.T) {
		// 2 of 4 jobs = 50% → cost should be ~0.52
		table := FormatCostTableSelected(estimates, []int{2, 0}, 0, 0)
		text := costTableText(table)
		if !strings.Contains(text, "0.52") {
			t.Errorf("should show scaled cost for half-selected group, got %q", text)
		}
	})
}

func TestFormatDurationWithBounds(t *testing.T) {
	tests := []struct {
		name string
		est  estimate.Estimate
		want string
	}{
		{"zero", estimate.Estimate{}, "—"},
		{"no bounds", estimate.Estimate{Mean: time.Hour, Lower: time.Hour, Upper: time.Hour}, "~1h"},
		{"with bounds", estimate.Estimate{Mean: 2*time.Hour + 15*time.Minute, Lower: time.Hour, Upper: 4 * time.Hour}, "~2h15 (1h–4h)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDurationWithBounds(tt.est)
			if got != tt.want {
				t.Errorf("formatDurationWithBounds = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatCostWithBounds(t *testing.T) {
	t.Run("zero cost", func(t *testing.T) {
		got := formatCostWithBounds(0, estimate.Estimate{}, 1.0)
		if got != "—" {
			t.Errorf("zero cost should return dash, got %q", got)
		}
	})

	t.Run("with spread", func(t *testing.T) {
		e := estimate.Estimate{Mean: 2 * time.Hour, Lower: 1 * time.Hour, Upper: 3 * time.Hour}
		got := formatCostWithBounds(2.0, e, 1.0)
		if !strings.Contains(got, "~$2.00") {
			t.Errorf("should contain mean cost, got %q", got)
		}
		if !strings.Contains(got, "$1.00–$3.00") {
			t.Errorf("should contain cost range, got %q", got)
		}
	})
}

func TestFormatCostBreakdown(t *testing.T) {
	estimates := []CostEstimate{
		{
			Group: InstanceGroup{GPUClass: "A100", GPUMemGB: 80, Jobs: make([]*db.Job, 3)},
			Offer: GroupOffer{Offer: &cloud.Offer{GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 0.52}},
			Breakdown: estimate.Breakdown{
				Startup:   estimate.Estimate{Mean: 45 * time.Second, Lower: 30 * time.Second, Upper: 3 * time.Minute},
				Provision: estimate.Estimate{Mean: 8 * time.Minute, Lower: 4 * time.Minute, Upper: 16 * time.Minute},
				Run:       estimate.Estimate{Mean: 2 * time.Hour, Lower: 1 * time.Hour, Upper: 4 * time.Hour},
				Total:     estimate.Estimate{Mean: 2*time.Hour + 9*time.Minute, Lower: 1*time.Hour + 5*time.Minute, Upper: 4*time.Hour + 19*time.Minute},
			},
			DownloadBytes: 12 * 1024 * 1024 * 1024,
			TotalCost:     1.04,
		},
	}

	result := FormatCostBreakdown(estimates, []int{2})
	if !strings.Contains(result, "Cost Breakdown") {
		t.Error("should contain header")
	}
	if !strings.Contains(result, "Instance startup") {
		t.Error("should contain startup phase")
	}
	if !strings.Contains(result, "Provisioning") {
		t.Error("should contain provision phase")
	}
	if !strings.Contains(result, "Job runtime") {
		t.Error("should contain run phase")
	}
	if !strings.Contains(result, "2 jobs, sequential") {
		t.Errorf("should show selected job count, got %q", result)
	}
	if !strings.Contains(result, "12 GB") {
		t.Errorf("should show download size, got %q", result)
	}
	if !strings.Contains(result, "press d to return") {
		t.Error("should contain back instruction")
	}
}

// costTableText joins all CostTable lines into a single string for assertion checks.
func costTableText(ct CostTable) string {
	var parts []string
	for _, cl := range ct.Lines {
		parts = append(parts, cl.Text)
	}
	return strings.Join(parts, "\n")
}

func TestSummarizeForComparison(t *testing.T) {
	estimates := []CostEstimate{
		{
			Group: InstanceGroup{GPUClass: "A100", GPUMemGB: 80, Jobs: make([]*db.Job, 4)},
			Offer: GroupOffer{Offer: &cloud.Offer{GPUName: "A100", CostPerHour: 1.50}},
			Breakdown: estimate.Breakdown{
				Total: estimate.Estimate{Mean: 2 * time.Hour, Lower: 1 * time.Hour, Upper: 4 * time.Hour},
			},
			TotalCost: 3.00,
		},
		{
			Group: InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24, Jobs: make([]*db.Job, 3)},
			Offer: GroupOffer{Offer: &cloud.Offer{GPUName: "RTX 4090", CostPerHour: 0.50}},
			Breakdown: estimate.Breakdown{
				Total: estimate.Estimate{Mean: 5 * time.Hour, Lower: 2 * time.Hour, Upper: 10 * time.Hour},
			},
			TotalCost: 2.50,
		},
	}

	t.Run("max time semantics", func(t *testing.T) {
		row := SummarizeForComparison(estimates, []int{4, 3})
		if row == nil {
			t.Fatal("expected non-nil row")
		}
		if row.NumGPUs != 2 {
			t.Errorf("NumGPUs = %d, want 2", row.NumGPUs)
		}
		// MaxTime should be max(2h, 5h) = 5h, not sum (7h)
		if row.MaxTime.Mean != 5*time.Hour {
			t.Errorf("MaxTime.Mean = %v, want 5h (max, not sum)", row.MaxTime.Mean)
		}
		// Cost should be sum
		expectedCost := 3.00 + 2.50
		if row.TotalCost < expectedCost-0.01 || row.TotalCost > expectedCost+0.01 {
			t.Errorf("TotalCost = %f, want ~%f", row.TotalCost, expectedCost)
		}
	})

	t.Run("selection scaling", func(t *testing.T) {
		// Select 2/4 of group 0, 0/3 of group 1
		row := SummarizeForComparison(estimates, []int{2, 0})
		if row == nil {
			t.Fatal("expected non-nil row")
		}
		// Group 0: 2/4 scale → time 1h, cost 1.50
		// Group 1: 0/3 scale → time 0, cost 0
		// Max time = 1h
		if row.MaxTime.Mean != 1*time.Hour {
			t.Errorf("MaxTime.Mean = %v, want 1h with partial selection", row.MaxTime.Mean)
		}
		if row.TotalCost < 1.49 || row.TotalCost > 1.51 {
			t.Errorf("TotalCost = %f, want ~1.50", row.TotalCost)
		}
	})

	t.Run("nil for empty", func(t *testing.T) {
		row := SummarizeForComparison(nil, nil)
		if row != nil {
			t.Error("expected nil for empty estimates")
		}
	})
}

func TestFormatStrategySummary(t *testing.T) {
	rows := []StrategySummaryRow{
		{
			Label:     "cheap",
			Active:    false,
			NumGPUs:   2,
			MaxTime:   estimate.Estimate{Mean: 8*time.Hour + 20*time.Minute, Lower: 2 * time.Hour, Upper: 24 * time.Hour},
			TotalCost: 0.61,
			CostLower: 0.16,
			CostUpper: 2.48,
		},
		{
			Label:     "fast",
			Active:    true,
			NumGPUs:   2,
			MaxTime:   estimate.Estimate{Mean: 2*time.Hour + 5*time.Minute, Lower: 30 * time.Minute, Upper: 6 * time.Hour},
			TotalCost: 2.42,
			CostLower: 0.60,
			CostUpper: 9.60,
		},
		{
			Label:     "fastest",
			Active:    false,
			NumGPUs:   2,
			MaxTime:   estimate.Estimate{Mean: 52 * time.Minute, Lower: 15 * time.Minute, Upper: 3 * time.Hour},
			TotalCost: 3.88,
			CostLower: 0.97,
			CostUpper: 15.50,
		},
	}

	table := FormatStrategySummary(rows)
	text := costTableText(table)

	// Should contain all strategy names
	if !strings.Contains(text, "cheap") {
		t.Error("should contain 'cheap'")
	}
	if !strings.Contains(text, "fast") {
		t.Error("should contain 'fast'")
	}
	if !strings.Contains(text, "fastest") {
		t.Error("should contain 'fastest'")
	}
	// Should have active marker (▸ when not disclosed)
	if !strings.Contains(text, "▸") {
		t.Error("should contain active marker ▸")
	}
	// Should have 3 lines
	if len(table.Lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(table.Lines))
	}
	// Active row should not be dimmed
	if table.Lines[1].Dimmed {
		t.Error("active row should not be dimmed")
	}
	// Others should be dimmed
	if !table.Lines[0].Dimmed {
		t.Error("inactive row should be dimmed")
	}
}

func TestFormatStrategySummary_Loading(t *testing.T) {
	rows := []StrategySummaryRow{
		{Label: "cheap", Active: true, NumGPUs: 1,
			MaxTime: estimate.Estimate{Mean: time.Hour}, TotalCost: 0.50},
		{Label: "fast", Active: false, Loading: true},
		{Label: "fastest", Active: false, Loading: true},
	}
	table := FormatStrategySummary(rows)
	text := costTableText(table)
	if !strings.Contains(text, "estimating...") {
		t.Error("loading row should show 'estimating...'")
	}
}

func TestFormatParetoSparkline(t *testing.T) {
	rows := []StrategySummaryRow{
		{Label: "cheap", Active: false,
			MaxTime:   estimate.Estimate{Mean: 8 * time.Hour, Lower: 2 * time.Hour, Upper: 24 * time.Hour},
			TotalCost: 0.61},
		{Label: "fast", Active: true,
			MaxTime:   estimate.Estimate{Mean: 2 * time.Hour, Lower: 30 * time.Minute, Upper: 6 * time.Hour},
			TotalCost: 2.42},
		{Label: "fastest", Active: false,
			MaxTime:   estimate.Estimate{Mean: 52 * time.Minute, Lower: 15 * time.Minute, Upper: 3 * time.Hour},
			TotalCost: 3.88},
	}

	sparkTable := FormatParetoSparkline(rows)
	if sparkTable == nil {
		t.Fatal("sparkline should not be nil with 3 points")
	}
	text := costTableText(*sparkTable)
	// Should contain strategy labels
	if !strings.Contains(text, "cheap") {
		t.Error("sparkline should label 'cheap'")
	}
	if !strings.Contains(text, "fast") {
		t.Error("sparkline should label 'fast'")
	}
	// Should contain point markers
	if !strings.Contains(text, "●") {
		t.Error("sparkline should contain point marker ●")
	}
	// Active row should not be dimmed
	hasActiveRow := false
	for _, l := range sparkTable.Lines {
		if !l.Dimmed && strings.Contains(l.Text, "●") {
			hasActiveRow = true
		}
	}
	if !hasActiveRow {
		t.Error("sparkline should have a non-dimmed row for the active strategy")
	}
	// Should contain error bar elements
	if !strings.Contains(text, "─") {
		t.Error("sparkline should contain error bar lines ─")
	}
	// Should contain axis elements
	if !strings.Contains(text, "└") {
		t.Error("sparkline should contain X-axis corner")
	}
	// Y-axis labels should show actual point costs, not padded axis bounds
	for _, cost := range []string{"$3.88", "$2.42", "$0.61"} {
		if !strings.Contains(text, cost) {
			t.Errorf("Y-axis should show actual cost %s, got:\n%s", cost, text)
		}
	}
}

func TestFormatParetoSparkline_TooFewPoints(t *testing.T) {
	rows := []StrategySummaryRow{
		{Label: "cheap", Active: true, MaxTime: estimate.Estimate{Mean: time.Hour}, TotalCost: 1.00},
	}
	if FormatParetoSparkline(rows) != nil {
		t.Error("should return nil with fewer than 2 points")
	}
}

func TestFormatParetoSparkline_WithLoading(t *testing.T) {
	rows := []StrategySummaryRow{
		{Label: "cheap", Active: true, MaxTime: estimate.Estimate{Mean: time.Hour, Lower: 30 * time.Minute, Upper: 2 * time.Hour}, TotalCost: 1.00},
		{Label: "fast", Active: false, Loading: true},
		{Label: "fastest", Active: false, MaxTime: estimate.Estimate{Mean: 30 * time.Minute, Lower: 15 * time.Minute, Upper: time.Hour}, TotalCost: 5.00},
	}
	sparkTable := FormatParetoSparkline(rows)
	if sparkTable == nil {
		t.Fatal("should render with 2 non-loading points")
	}
}
