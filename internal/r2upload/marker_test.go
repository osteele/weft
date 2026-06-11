package r2upload

import (
	"encoding/json"
	"testing"
)

// Round-trip: the StallKind computed by Drain must survive
// NewFailureMarker → JSON → FailureMarker so displays can discriminate
// between stall kinds.
func TestFailureMarkerRoundTripPreservesKind(t *testing.T) {
	kinds := []StallKind{
		StallKindNeverStarted, StallKindMidTransfer,
		StallKindHeartbeat, StallKindSlowPace,
	}
	for _, kind := range kinds {
		r := Result{
			Status:    StatusStalled,
			KilledBy:  KilledByStall,
			Reason:    "no progress",
			StallKind: kind,
		}
		m := NewFailureMarker(r)
		if m.Kind != kind {
			t.Errorf("NewFailureMarker Kind = %q, want %q", m.Kind, kind)
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal (%s): %v", kind, err)
		}
		var got FailureMarker
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal (%s): %v", kind, err)
		}
		if got.Kind != kind {
			t.Errorf("round-trip Kind = %q, want %q", got.Kind, kind)
		}
	}
}

// Markers written by older agents have no stall_kind field. They must still
// parse, with an empty Kind, so displays fall back to the generic stall.
func TestFailureMarkerOldFormatWithoutKindParses(t *testing.T) {
	old := `{
		"status": "stalled",
		"reason": "no progress for 30s",
		"killed_by": "stall",
		"bytes_uploaded": 123,
		"bytes_total": 1000,
		"elapsed_seconds": 31.5,
		"started_at_unix": 1700000000,
		"ended_at_unix": 1700000031,
		"ceiling_seconds": 900,
		"stall_timeout_seconds": 30,
		"job_id": 7
	}`
	var m FailureMarker
	if err := json.Unmarshal([]byte(old), &m); err != nil {
		t.Fatalf("old-format marker failed to parse: %v", err)
	}
	if m.Kind != "" {
		t.Errorf("Kind = %q, want empty for old-format marker", m.Kind)
	}
	if m.Status != StatusStalled || m.KilledBy != KilledByStall {
		t.Errorf("Status/KilledBy = %q/%q, want stalled/stall", m.Status, m.KilledBy)
	}
	if m.JobID != 7 || m.BytesUploaded != 123 {
		t.Errorf("JobID/BytesUploaded = %d/%d, want 7/123", m.JobID, m.BytesUploaded)
	}
}

func TestCauseSurfacesStallKind(t *testing.T) {
	cases := []struct {
		kind StallKind
		want string
	}{
		{StallKindNeverStarted, "stall: never_started"},
		{StallKindMidTransfer, "stall: mid_transfer"},
		{StallKindHeartbeat, "stall: heartbeat"},
	}
	for _, c := range cases {
		m := FailureMarker{KilledBy: KilledByStall, Kind: c.kind}
		if got := m.Cause(); got != c.want {
			t.Errorf("Cause(kind=%s) = %q, want %q", c.kind, got, c.want)
		}
	}
}

func TestCauseCollapsesRedundantKind(t *testing.T) {
	// slow_pace kills carry KilledBy == Kind; don't print "slow_pace: slow_pace".
	m := FailureMarker{
		KilledBy: KilledBySlowPace,
		Kind:     StallKindSlowPace,
	}
	if got := m.Cause(); got != "slow_pace" {
		t.Errorf("Cause = %q, want %q", got, "slow_pace")
	}
}

// Old-format markers (written before stall_kind existed) must parse and
// display as the generic "stall" token.
func TestCauseOldFormatMarkerGenericStall(t *testing.T) {
	old := `{"status":"stalled","reason":"no progress for 30s","killed_by":"stall","bytes_uploaded":1,"bytes_total":2,"elapsed_seconds":30,"started_at_unix":0,"ended_at_unix":30,"ceiling_seconds":900,"stall_timeout_seconds":30}`
	var m FailureMarker
	if err := json.Unmarshal([]byte(old), &m); err != nil {
		t.Fatalf("old-format marker failed to parse: %v", err)
	}
	if got := m.Cause(); got != "stall" {
		t.Errorf("Cause(old marker) = %q, want %q", got, "stall")
	}
}

func TestCauseNonStall(t *testing.T) {
	m := FailureMarker{KilledBy: KilledByCeiling}
	if got := m.Cause(); got != "ceiling" {
		t.Errorf("Cause = %q, want %q", got, "ceiling")
	}
}

// Non-stall failures (ceiling, error) have no StallKind; the marker must
// omit the field rather than emit an empty string.
func TestFailureMarkerOmitsEmptyKind(t *testing.T) {
	m := NewFailureMarker(Result{Status: StatusCeiling, KilledBy: KilledByCeiling})
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, present := raw["stall_kind"]; present {
		t.Errorf("stall_kind present in JSON for non-stall marker: %s", data)
	}
}
