package dashboard

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestGPUColumnText(t *testing.T) {
	intPtr := func(v int) *int { return &v }

	tests := []struct {
		name string
		job  db.Job
		want string
	}{
		{
			name: "GPUClass set",
			job:  db.Job{GPUClass: "A100"},
			want: "A100",
		},
		{
			name: "GPUClass with generation suffix",
			job:  db.Job{GPUClass: "ampere+"},
			want: "ampere+",
		},
		{
			name: "GPUMemGB only",
			job:  db.Job{GPUMemGB: intPtr(24)},
			want: "≥24GB",
		},
		{
			name: "GPUClass takes priority over GPUMemGB",
			job:  db.Job{GPUClass: "A100", GPUMemGB: intPtr(80)},
			want: "A100",
		},
		{
			name: "neither set",
			job:  db.Job{},
			want: "—",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gpuColumnText(&tt.job)
			if got != tt.want {
				t.Errorf("gpuColumnText() = %q, want %q", got, tt.want)
			}
		})
	}
}
