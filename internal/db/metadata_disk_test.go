package db

import "testing"

func TestJobDiskMetadataIsEmpty(t *testing.T) {
	tests := []struct {
		name string
		disk *JobDiskMetadata
		want bool
	}{
		{name: "nil", disk: nil, want: true},
		{name: "zero", disk: &JobDiskMetadata{}, want: true},
		{name: "floor only", disk: &JobDiskMetadata{DiskGB: 120}, want: false},
		{name: "ceiling only", disk: &JobDiskMetadata{DiskMaxGB: 90}, want: false},
		{name: "runtime only", disk: &JobDiskMetadata{RuntimeDiskGB: 24}, want: false},
		{
			// EstimatedRuntimeDiskGB is a cached estimate, not user intent, so
			// a record carrying only that declares nothing.
			name: "estimated runtime only",
			disk: &JobDiskMetadata{EstimatedRuntimeDiskGB: 24},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.disk.IsEmpty(); got != tt.want {
				t.Fatalf("IsEmpty() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJobDiskMetadataEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b *JobDiskMetadata
		want bool
	}{
		{name: "nil and nil", a: nil, b: nil, want: true},
		{name: "nil and zero", a: nil, b: &JobDiskMetadata{}, want: true},
		{name: "nil and declared", a: nil, b: &JobDiskMetadata{DiskGB: 120}, want: false},
		{
			name: "same floor",
			a:    &JobDiskMetadata{DiskGB: 120},
			b:    &JobDiskMetadata{DiskGB: 120},
			want: true,
		},
		{
			// A changed ceiling must not compare equal, or the new value is
			// silently discarded by callers that skip writes on equality.
			name: "ceiling differs",
			a:    &JobDiskMetadata{DiskGB: 120, DiskMaxGB: 90},
			b:    &JobDiskMetadata{DiskGB: 120, DiskMaxGB: 30},
			want: false,
		},
		{
			name: "ceiling added",
			a:    &JobDiskMetadata{DiskGB: 120},
			b:    &JobDiskMetadata{DiskGB: 120, DiskMaxGB: 90},
			want: false,
		},
		{
			name: "ceiling cleared",
			a:    &JobDiskMetadata{DiskMaxGB: 90},
			b:    &JobDiskMetadata{},
			want: false,
		},
		{
			name: "runtime differs",
			a:    &JobDiskMetadata{DiskGB: 120, RuntimeDiskGB: 24},
			b:    &JobDiskMetadata{DiskGB: 120, RuntimeDiskGB: 8},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Fatalf("Equal() = %v, want %v", got, tt.want)
			}
			if got := tt.b.Equal(tt.a); got != tt.want {
				t.Fatalf("Equal() reversed = %v, want %v", got, tt.want)
			}
		})
	}
}
