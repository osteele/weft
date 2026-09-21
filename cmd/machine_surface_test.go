package cmd

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// jsonFieldSet returns the JSON key names a struct type declares, including
// keys carrying omitempty. It reads the declared shape rather than a marshalled
// document so that an omitted-when-empty field is still pinned.
func jsonFieldSet(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	if typ.Kind() != reflect.Struct {
		t.Fatalf("jsonFieldSet: %T is not a struct", v)
	}
	var names []string
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		names = append(names, strings.Split(tag, ",")[0])
	}
	sort.Strings(names)
	return names
}

// assertJSONFieldSet pins a machine surface's declared key set. A rename or a
// removal is a breaking change for a consumer that refuses unrecognized shapes,
// and the version integer is bumped by hand — so without this the version can
// stay at 1 through a break. Updating the expected list here is the prompt to
// ask whether the surface's version must be bumped too.
func assertJSONFieldSet(t *testing.T, surface string, v any, want []string) {
	t.Helper()
	got := jsonFieldSet(t, v)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s field set changed.\n got: %q\nwant: %q\n\nIf this change is intentional, update the expected set AND decide whether it breaks a reader that refuses unrecognized shapes — if so, bump the surface version.", surface, got, want)
	}
}

func TestMachineSurfaceFieldSets(t *testing.T) {
	// "source" appears only in bytes rendered on an edge (omitted on the hub):
	// additive, so no version bump.
	assertJSONFieldSet(t, "job_list envelope", jobListJSONEnvelope{}, []string{"jobs", "kind", "selection", "source", "version"})
	assertJSONFieldSet(t, "job_list selection", jobListJSONSelection{}, []string{"complete", "constraints", "order"})
	assertJSONFieldSet(t, "job_list constraint", jobListJSONConstraint{}, []string{"hidden", "kind", "requested", "value"})
	assertJSONFieldSet(t, "host_list envelope", hostListJSONEnvelope{}, []string{"hosts", "kind", "source", "version"})
	assertJSONFieldSet(t, "host_list row", hostListJSONRow{}, []string{"capabilities", "capability_observations", "name", "ssh_identity_file", "ssh_target", "type"})
	assertJSONFieldSet(t, "autopilot_status document", autopilotStateView{}, []string{
		"active_binary_mtime", "active_binary_path", "active_binary_size", "active_binary_stale", "active_runner_host",
		"active_runner_label", "active_runner_pid",
		"blocked_on_price_authorization", "heartbeat_age_seconds",
		"heartbeat_at", "incidents", "kind", "last_pass_duration_ms",
		"last_pass_error", "last_pass_finished_at", "last_pass_summary",
		"orphan_streaks", "pass_age_seconds", "pass_started_at", "paused",
		"paused_at", "paused_by", "paused_reason", "source", "stale_after_seconds",
		"state", "version",
	})
	assertJSONFieldSet(t, "autopilot_status price block", priceAuthBlockView{}, []string{
		"authorize_command", "description", "gpu_bucket", "gpu_class",
		"gpu_mem_gb", "job_id", "offered_cents", "project", "reason",
	})
	assertJSONFieldSet(t, "autopilot_status orphan streak", orphanStreakView{}, []string{"description", "job_id", "orphan_count", "project"})
	assertJSONFieldSet(t, "autopilot_blocked envelope", autopilotBlockedEnvelope{}, []string{"kind", "scopes", "version"})
	assertJSONFieldSet(t, "autopilot_blocked scope", blockedScopeView{}, []string{
		"age_seconds", "campaign_id", "chain", "infra_failures", "jobs",
		"orphaned", "project", "scope", "spend_cents", "tripped_at", "window",
	})
	assertJSONFieldSet(t, "job_telemetry envelope", telemetryEnvelope{}, []string{"kind", "results", "version"})
	assertJSONFieldSet(t, "job_telemetry result", telemetryOutput{}, []string{
		"attempt_id", "duration_s", "gpu", "job", "job_id", "raw_samples",
		"samples", "summary", "time_max", "time_min",
	})
}

// Every versioned surface must emit kind and version, including for an empty
// result — a document carrying no data still carries its identity, and a
// consumer must be able to recognize the shape before trusting its emptiness.
func TestMachineSurfaceEnvelopesCarryIdentityWhenEmpty(t *testing.T) {
	for _, tc := range []struct {
		surface string
		value   any
		want    string
	}{
		{"autopilot_blocked", autopilotBlockedEnvelope{
			Kind: autopilotBlockedJSONKind, Version: autopilotBlockedJSONVersion,
			Scopes: []blockedScopeView{},
		}, `{"kind":"autopilot_blocked","version":1,"scopes":[]}`},
		{"job_telemetry", telemetryEnvelope{
			Kind: telemetryJSONKind, Version: telemetryJSONVersion,
			Results: []telemetryOutput{},
		}, `{"kind":"job_telemetry","version":1,"results":[]}`},
	} {
		t.Run(tc.surface, func(t *testing.T) {
			data, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(data) != tc.want {
				t.Errorf("got  %s\nwant %s", data, tc.want)
			}
		})
	}
}
