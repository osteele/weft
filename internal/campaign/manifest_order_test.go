package campaign

import (
	"reflect"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestOrderAgentJobsForManifest(t *testing.T) {
	tests := []struct {
		name string
		jobs []cloud.AgentJob
		want []int64
	}{
		{
			name: "high-priority consumer follows low-priority producer",
			jobs: []cloud.AgentJob{
				{ID: 20, Priority: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 10}}},
				{ID: 10, Priority: 1},
			},
			want: []int64{10, 20},
		},
		{
			name: "unrelated jobs keep scheduling order",
			jobs: []cloud.AgentJob{
				{ID: 20, Priority: 10},
				{ID: 10, Priority: 1},
			},
			want: []int64{20, 10},
		},
		{
			name: "external ref does not reorder",
			jobs: []cloud.AgentJob{
				{ID: 20, Priority: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 99}}},
				{ID: 10, Priority: 1},
			},
			want: []int64{20, 10},
		},
		{
			name: "duplicate refs impose one edge",
			jobs: []cloud.AgentJob{
				{ID: 20, Priority: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 10}, {JobID: 10}}},
				{ID: 10, Priority: 1},
				{ID: 30, Priority: 0},
			},
			want: []int64{10, 20, 30},
		},
		{
			name: "self ref does not reorder",
			jobs: []cloud.AgentJob{
				{ID: 20, Priority: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 20}}},
				{ID: 10, Priority: 1},
			},
			want: []int64{20, 10},
		},
		{
			name: "cycle emits every job once",
			jobs: []cloud.AgentJob{
				{ID: 20, Priority: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 10}}},
				{ID: 10, Priority: 1, CloudAfter: []cloud.CloudAfterRef{{JobID: 20}}},
				{ID: 30, Priority: 0},
			},
			want: []int64{10, 20, 30},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orderAgentJobsForManifest(tt.jobs)
			if got := agentJobIDs(tt.jobs); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("order = %v, want %v", got, tt.want)
			}
		})
	}
}

func agentJobIDs(jobs []cloud.AgentJob) []int64 {
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.ID)
	}
	return ids
}
