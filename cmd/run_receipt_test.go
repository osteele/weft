package cmd

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

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
	if err := emitRunReceipt(cmd, runReceiptForJob(job, "accepted_immediately", true, false, "assignment-7")); err != nil {
		t.Fatal(err)
	}
	var got runSubmissionReceipt
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("receipt is not JSON: %v", err)
	}
	if got.APIVersion != runReceiptAPIVersion || got.JobID != "wj42" || got.SourcePin != "sha256:source" || !got.AcceptedImmediately {
		t.Fatalf("receipt = %+v", got)
	}
}
