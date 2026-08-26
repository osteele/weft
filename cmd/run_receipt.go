package cmd

import (
	"encoding/json"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostcap"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

const runReceiptAPIVersion = "weft.run.receipt.v1"

type runSubmissionReceipt struct {
	APIVersion          string                         `json:"api_version"`
	JobID               string                         `json:"job_id,omitempty"`
	PlacementDecision   string                         `json:"placement_decision"`
	SelectedHost        string                         `json:"selected_host,omitempty"`
	SourcePin           string                         `json:"source_pin,omitempty"`
	SourceIdentityKind  string                         `json:"source_identity_kind,omitempty"`
	ExecutionSource     *db.JobSourceExecutionMetadata `json:"execution_source,omitempty"`
	AcceptedImmediately bool                           `json:"accepted_immediately"`
	Deduplicated        bool                           `json:"deduplicated,omitempty"`
	IdempotencyKey      string                         `json:"idempotency_key,omitempty"`
}

func emitRunReceipt(cmd *cobra.Command, receipt runSubmissionReceipt) error {
	receipt.APIVersion = runReceiptAPIVersion
	return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
}

func runReceiptForJob(job *db.Job, decision string, accepted, deduplicated bool, key string) runSubmissionReceipt {
	receipt := runSubmissionReceipt{
		PlacementDecision:   decision,
		AcceptedImmediately: accepted,
		Deduplicated:        deduplicated,
		IdempotencyKey:      key,
	}
	if job == nil {
		return receipt
	}
	receipt.JobID = ids.FormatJobID(job.ID)
	receipt.SelectedHost = job.Host
	if job.Metadata != nil && job.Metadata.Source != nil {
		if job.Metadata.Source.Pin != nil {
			receipt.SourcePin = job.Metadata.Source.Pin.Hash
		}
		if receipt.SourcePin == "" {
			receipt.SourcePin = job.Metadata.Source.Hash
		}
		if job.Metadata.Source.Pin != nil {
			receipt.SourceIdentityKind = db.SourceIdentityManifestV2
		}
		if job.Metadata.Source.Execution != nil {
			execution := *job.Metadata.Source.Execution
			receipt.ExecutionSource = &execution
		} else if job.Metadata.Source.Pin != nil {
			receipt.ExecutionSource = &db.JobSourceExecutionMetadata{
				SubmittedIdentityKind: db.SourceIdentityManifestV2,
				SubmittedSHA256:       job.Metadata.Source.Pin.Hash,
				DispatchMode:          plannedSourceDispatchMode(job),
				IdentityKind:          db.SourceIdentityManifestV2,
				DispatchedSHA256:      job.Metadata.Source.Pin.Hash,
				Verification:          db.SourceVerificationPending,
				RootCount:             len(job.Metadata.Source.Pin.Roots),
			}
		}
	}
	return receipt
}

func plannedSourceDispatchMode(job *db.Job) string {
	if job == nil || (job.LaunchID == nil && strings.TrimSpace(job.Host) == "") {
		return "pending_placement"
	}
	if job != nil && (job.LaunchID != nil || db.IsLaunchHost(job.Host)) {
		return "pinned_cloud_manifest"
	}
	return "pinned_inventory_manifest"
}

func normalizeRunCapabilities(agent string, capabilities []string) ([]string, error) {
	return hostcap.Normalize(agent, capabilities)
}

func sourcePinFromMetadata(source *db.JobSourceMetadata) string {
	if source == nil {
		return ""
	}
	if source.Pin != nil && source.Pin.Hash != "" {
		return source.Pin.Hash
	}
	return source.Hash
}
