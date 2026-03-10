package campaign

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestMatchJobToInstance_GPUClass(t *testing.T) {
	tests := []struct {
		name         string
		jobClass     string
		instClass    string
		instResolved string
		want         bool
	}{
		{"exact match", "A100", "A100", "", true},
		{"case insensitive", "a100", "A100", "", true},
		{"mismatch", "A100", "RTX_4090", "", false},
		{"empty job class matches anything", "", "A100", "", true},
		{"match against resolved name", "A100", "RTX_4090", "A100", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &db.Job{GPUClass: tt.jobClass}
			cap := InstanceCapacity{
				Instance: &db.CloudInstance{
					GPUClass:        tt.instClass,
					ResolvedGPUName: tt.instResolved,
					GPUMemGB:        80,
				},
				DiskFreeGB: 100,
			}
			got, reason := MatchJobToInstance(job, cap)
			if got != tt.want {
				t.Errorf("MatchJobToInstance() = %v, want %v (reason: %s)", got, tt.want, reason)
			}
		})
	}
}

func TestMatchJobToInstance_GPUMemory(t *testing.T) {
	mem24 := 24
	mem80 := 80

	tests := []struct {
		name      string
		jobMemGB  *int
		instMemGB int
		want      bool
	}{
		{"no job mem requirement", nil, 24, true},
		{"exact match", &mem24, 24, true},
		{"sufficient", &mem24, 80, true},
		{"insufficient", &mem80, 24, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &db.Job{GPUMemGB: tt.jobMemGB}
			cap := InstanceCapacity{
				Instance:   &db.CloudInstance{GPUMemGB: tt.instMemGB},
				DiskFreeGB: 100,
			}
			got, reason := MatchJobToInstance(job, cap)
			if got != tt.want {
				t.Errorf("MatchJobToInstance() = %v, want %v (reason: %s)", got, tt.want, reason)
			}
		})
	}
}

func TestMatchJobToInstance_DiskCompatibility(t *testing.T) {
	tests := []struct {
		name              string
		jobInputs         []string
		provisionedInputs []string
		diskFreeGB        int
		instDiskGB        int
		want              bool
	}{
		{"no inputs needed", nil, nil, 50, 100, true},
		{"all inputs already cached", []string{"hf:model-a"}, []string{"hf:model-a"}, 0, 100, true},
		{"no disk info skips check", []string{"hf:model-a"}, nil, 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &db.Job{Inputs: tt.jobInputs}
			cap := InstanceCapacity{
				Instance:          &db.CloudInstance{DiskGB: tt.instDiskGB},
				ProvisionedInputs: tt.provisionedInputs,
				DiskFreeGB:        tt.diskFreeGB,
			}
			got, _ := MatchJobToInstance(job, cap)
			if got != tt.want {
				t.Errorf("MatchJobToInstance() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchJobToInstance_GraceDeadline(t *testing.T) {
	job := &db.Job{}

	// Grace with enough time remaining
	cap := InstanceCapacity{
		Instance:       &db.CloudInstance{Status: db.CloudInstanceStatusGrace},
		GraceRemaining: 10 * time.Minute,
		DiskFreeGB:     50,
	}
	got, _ := MatchJobToInstance(job, cap)
	if !got {
		t.Error("expected compatible with 10m grace remaining")
	}

	// Grace with too little time
	cap.GraceRemaining = 2 * time.Minute
	got, _ = MatchJobToInstance(job, cap)
	if got {
		t.Error("expected incompatible with 2m grace remaining")
	}

	// Running instance (no grace concern)
	cap.GraceRemaining = 0
	got, _ = MatchJobToInstance(job, cap)
	if !got {
		t.Error("expected compatible with running instance")
	}
}

func TestRankForJob(t *testing.T) {
	job := &db.Job{
		Inputs: []string{"hf:model-a", "hf:model-b"},
	}

	graceInstance := InstanceCapacity{
		Instance: &db.CloudInstance{
			ID:       1,
			Status:   db.CloudInstanceStatusGrace,
			GPUMemGB: 80,
		},
		GraceRemaining:    10 * time.Minute,
		ProvisionedInputs: []string{"hf:model-a"},
		DiskFreeGB:        100,
	}

	runningInstance := InstanceCapacity{
		Instance: &db.CloudInstance{
			ID:       2,
			Status:   db.CloudInstanceStatusRunning,
			GPUMemGB: 80,
		},
		ProvisionedInputs: []string{"hf:model-a", "hf:model-b"},
		DiskFreeGB:        100,
	}

	ranked := RankForJob(job, []InstanceCapacity{runningInstance, graceInstance})

	if len(ranked) != 2 {
		t.Fatalf("expected 2 results, got %d", len(ranked))
	}

	// Grace should come first (free)
	if ranked[0].Instance.ID != 1 {
		t.Errorf("expected grace instance first, got instance %d", ranked[0].Instance.ID)
	}
}

func TestSubtractInputs(t *testing.T) {
	tests := []struct {
		name        string
		jobInputs   []string
		provisioned []string
		want        int
	}{
		{"all new", []string{"a", "b"}, nil, 2},
		{"all cached", []string{"a", "b"}, []string{"a", "b"}, 0},
		{"partial overlap", []string{"a", "b", "c"}, []string{"a"}, 2},
		{"empty job", nil, []string{"a"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := subtractInputs(tt.jobInputs, tt.provisioned)
			if len(result) != tt.want {
				t.Errorf("subtractInputs() returned %d items, want %d", len(result), tt.want)
			}
		})
	}
}
