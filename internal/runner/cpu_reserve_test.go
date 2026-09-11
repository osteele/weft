package runner

import (
	"testing"

	"github.com/osteele/weft/internal/opsqueue"
)

func TestReserveCoresAllotment(t *testing.T) {
	tests := []struct {
		name     string
		cores    int
		cpuCount int
		want     int
	}{
		{"zero reserve", 0, 12, 0},
		{"negative reserve", -2, 12, 0},
		{"exact division", 3, 12, 25},
		{"ceiling rounds up", 2, 12, 17}, // 16.67 -> 17
		{"one of many", 1, 32, 4},        // 3.125 -> 4
		{"whole host", 12, 12, 100},
		{"over host runs alone", 15, 12, 125},
		{"no cpus reserves whole host", 4, 0, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReserveCoresAllotment(tt.cores, tt.cpuCount); got != tt.want {
				t.Errorf("ReserveCoresAllotment(%d, %d) = %d, want %d", tt.cores, tt.cpuCount, got, tt.want)
			}
		})
	}
}

func TestJobAllotmentReserveCoresPrecedence(t *testing.T) {
	r := &Runner{cpuConfig: DefaultCPUConfig(), cpuCount: 12}
	percent := 20

	tests := []struct {
		name string
		job  opsqueue.CommandJob
		want int
	}{
		{
			name: "explicit percent wins over reserve cores",
			job:  opsqueue.CommandJob{CPU: &percent, CPUReserveCores: 8},
			want: 20,
		},
		{
			name: "reserve cores normalize on this host",
			job:  opsqueue.CommandJob{CPUReserveCores: 2},
			want: 17,
		},
		{
			name: "GPU default applies without explicit cpu fields",
			job:  opsqueue.CommandJob{GPUClass: "a100"},
			want: r.cpuConfig.DefaultGPUAllotment(12),
		},
		{
			name: "ordinary default applies without explicit cpu fields",
			job:  opsqueue.CommandJob{},
			want: r.cpuConfig.DefaultAllotment(12),
		},
		{
			name: "zero reserve falls through to GPU default",
			job:  opsqueue.CommandJob{CPUReserveCores: 0, GPUMem: gpuMemPtr(24)},
			want: r.cpuConfig.DefaultGPUAllotment(12),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.jobAllotment(&tt.job); got != tt.want {
				t.Errorf("jobAllotment() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestReserveCoresAllotmentHostRelative(t *testing.T) {
	// The same absolute reservation normalizes differently per host: the
	// percent consumed by the gate is a property of the destination, not of
	// the submitting machine.
	if ReserveCoresAllotment(2, 12) == ReserveCoresAllotment(2, 48) {
		t.Fatal("2-core reservation normalized identically on 12- and 48-core hosts")
	}
}
