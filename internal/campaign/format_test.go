package campaign

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
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

	table := FormatCostTable(offers)
	if !strings.Contains(table, "A100") {
		t.Errorf("cost table should contain GPU class, got %q", table)
	}
	if !strings.Contains(table, "0.45") {
		t.Errorf("cost table should contain cost, got %q", table)
	}
	if !strings.Contains(table, "Total") {
		t.Errorf("cost table should contain total, got %q", table)
	}
}

func TestFormatCostTable_NoOffers(t *testing.T) {
	offers := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "A100", GPUMemGB: 80},
			Offer: nil,
		},
	}
	table := FormatCostTable(offers)
	if !strings.Contains(table, "No offers") {
		t.Errorf("should show no offers message, got %q", table)
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
