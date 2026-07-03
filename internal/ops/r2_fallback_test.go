package ops

import "testing"

func TestParseCompletionRecordTimes(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantStart int64
		wantEnd   int64
	}{
		{
			name:      "both times present",
			data:      `{"exit_code":0,"start_time":1700000001,"end_time":1700000011}`,
			wantStart: 1700000001,
			wantEnd:   1700000011,
		},
		{
			name:    "start_time absent (legacy record)",
			data:    `{"exit_code":0,"end_time":1700000011}`,
			wantEnd: 1700000011,
		},
		{
			name: "malformed json",
			data: `not json`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := parseCompletionRecordTimes([]byte(tt.data))
			if start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("parseCompletionRecordTimes = (%d, %d), want (%d, %d)", start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}
