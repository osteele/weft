package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

func TestNormalizeRunCapabilities(t *testing.T) {
	got, err := normalizeRunCapabilities("Codex", []string{"service:r2", "agent:codex"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"service:r2", "agent:codex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func TestEmitRunReceiptIsVersionedJSON(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	job := &db.Job{
		ID:   42,
		Host: "studio",
		Metadata: &db.JobMetadata{Source: &db.JobSourceMetadata{
			Pin: &db.JobSourcePinMetadata{Hash: "sha256:source"},
		}},
	}
	if err := emitRunReceiptForJob(cmd, nil, job, "accepted_immediately", true, false, "assignment-7"); err != nil {
		t.Fatal(err)
	}
	var got runSubmissionReceipt
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("receipt is not JSON: %v", err)
	}
	if got.APIVersion != runReceiptAPIVersion || got.JobID != "wj42" || got.SourcePin != "sha256:source" || !got.AcceptedImmediately {
		t.Fatalf("receipt = %+v", got)
	}
	if got.SourceIdentityKind != db.SourceIdentityManifestV2 || got.ExecutionSource == nil || got.ExecutionSource.DispatchMode != "pinned_inventory_manifest" || got.ExecutionSource.Verification != db.SourceVerificationPending {
		t.Fatalf("source receipt = %+v", got)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["rejection"]; present {
		t.Fatal("accepted v1 receipt acquired a rejection field")
	}
}

func TestEmitRunReceiptRejectsUnsafeConstruction(t *testing.T) {
	for _, tt := range []struct {
		name    string
		receipt runSubmissionReceipt
	}{
		{"durable job", runSubmissionReceipt{PlacementDecision: "not_accepted", JobID: "wj42"}},
		{"accepted flag", runSubmissionReceipt{PlacementDecision: "not_accepted", AcceptedImmediately: true}},
		{"deduplicated flag", runSubmissionReceipt{PlacementDecision: "not_accepted", Deduplicated: true}},
		{"accepted rejection", runSubmissionReceipt{PlacementDecision: "accepted_immediately", Rejection: &runRejection{Code: "host_offline"}}},
		{"queued rejection", runSubmissionReceipt{PlacementDecision: "queued", Rejection: &runRejection{Code: "host_offline"}}},
		{"deduplicated rejection", runSubmissionReceipt{PlacementDecision: "deduplicated", Rejection: &runRejection{Code: "host_offline"}}},
		{"empty code", runSubmissionReceipt{PlacementDecision: "not_accepted", Rejection: &runRejection{}}},
		{"invalid code unicode", runSubmissionReceipt{PlacementDecision: "not_accepted", Rejection: &runRejection{Code: "\xff"}}},
		{"invalid detail unicode", runSubmissionReceipt{PlacementDecision: "not_accepted", Rejection: &runRejection{Code: "host_offline", Detail: "\xff"}}},
		{"oversized detail", runSubmissionReceipt{PlacementDecision: "not_accepted", Rejection: &runRejection{Code: "host_offline", Detail: strings.Repeat("x", 1025)}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := emitRunReceipt(cmd, tt.receipt); err == nil {
				t.Fatal("unsafe receipt was accepted")
			}
			if out.Len() != 0 {
				t.Fatalf("unsafe receipt reached stdout: %s", &out)
			}
		})
	}
}

func TestRunRejectionDetailUnicodeBoundary(t *testing.T) {
	for _, tt := range []struct {
		name, detail, want string
	}{
		{"exact boundary", strings.Repeat("x", 1020) + "\U00010400", strings.Repeat("x", 1020) + "\U00010400"},
		{"split rune", strings.Repeat("x", 1023) + "\U00010400suffix", strings.Repeat("x", 1023)},
		{"invalid input", "host \xff offline", "host \uFFFD offline"},
		{"replacement at boundary", strings.Repeat("x", 1023) + "\xff", strings.Repeat("x", 1023)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			receipt := runSubmissionReceipt{PlacementDecision: "not_accepted", Rejection: newRunRejection("future_code", errors.New(tt.detail))}
			if err := emitRunReceipt(cmd, receipt); err != nil {
				t.Fatal(err)
			}
			var got runSubmissionReceipt
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Rejection == nil || got.Rejection.Code != "future_code" || got.Rejection.Detail != tt.want || !utf8.ValidString(got.Rejection.Detail) || len(got.Rejection.Detail) > 1024 {
				t.Fatalf("rejection = %+v, want detail %q", got.Rejection, tt.want)
			}
		})
	}
}
