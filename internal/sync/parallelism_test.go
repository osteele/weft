package sync

import "testing"

func TestBoundedSourceWorkers(t *testing.T) {
	tests := []struct {
		name  string
		procs int
		load  float64
		want  int
	}{
		{name: "single CPU", procs: 1, want: 1},
		{name: "idle four CPUs", procs: 4, want: 2},
		{name: "idle sixteen CPUs", procs: 16, want: 8},
		{name: "moderate competing load", procs: 16, load: 10.1, want: 4},
		{name: "nearly saturated", procs: 16, load: 15, want: 1},
		{name: "oversubscribed", procs: 8, load: 20, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundedSourceWorkers(tc.procs, tc.load); got != tc.want {
				t.Fatalf("boundedSourceWorkers(%d, %.1f) = %d, want %d", tc.procs, tc.load, got, tc.want)
			}
		})
	}
}
