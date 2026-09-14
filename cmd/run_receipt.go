package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostcap"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

const runReceiptAPIVersion = "weft.run.receipt.v1"
const runRejectionDetailMaxBytes = 1024

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
	Payloads            []runPayloadReceipt            `json:"payloads,omitempty"`
	Rejection           *runRejection                  `json:"rejection,omitempty"`
}

type runPayloadReceipt struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type runRejection struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func newRunRejection(code string, err error) *runRejection {
	detail := strings.ToValidUTF8(err.Error(), "\uFFFD")
	if len(detail) > runRejectionDetailMaxBytes {
		end := runRejectionDetailMaxBytes
		for !utf8.RuneStart(detail[end]) {
			end--
		}
		detail = detail[:end]
	}
	return &runRejection{Code: code, Detail: detail}
}

func emitRunReceipt(cmd *cobra.Command, receipt runSubmissionReceipt) error {
	if receipt.PlacementDecision == "not_accepted" && (receipt.JobID != "" || receipt.AcceptedImmediately || receipt.Deduplicated) {
		return fmt.Errorf("invalid run receipt: not_accepted cannot identify an accepted or durable job")
	}
	if rejection := receipt.Rejection; rejection != nil {
		if receipt.PlacementDecision != "not_accepted" {
			return fmt.Errorf("invalid run receipt: rejection requires not_accepted")
		}
		if rejection.Code == "" || !utf8.ValidString(rejection.Code) {
			return fmt.Errorf("invalid run receipt: rejection code must be nonempty valid UTF-8")
		}
		if !utf8.ValidString(rejection.Detail) || len(rejection.Detail) > runRejectionDetailMaxBytes {
			return fmt.Errorf("invalid run receipt: rejection detail must be valid UTF-8 and at most %d bytes", runRejectionDetailMaxBytes)
		}
	}
	receipt.APIVersion = runReceiptAPIVersion
	return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
}

func emitRunReceiptForJob(cmd *cobra.Command, database *sql.DB, job *db.Job, decision string, accepted, deduplicated bool, key string) error {
	receipt, err := runReceiptForJob(database, job, decision, accepted, deduplicated, key)
	if err != nil {
		return err
	}
	return emitRunReceipt(cmd, receipt)
}

func runReceiptForJob(database *sql.DB, job *db.Job, decision string, accepted, deduplicated bool, key string) (runSubmissionReceipt, error) {
	receipt := runSubmissionReceipt{
		PlacementDecision:   decision,
		AcceptedImmediately: accepted,
		Deduplicated:        deduplicated,
		IdempotencyKey:      key,
	}
	if job == nil {
		return receipt, nil
	}
	receipt.JobID = ids.FormatJobID(job.ID)
	receipt.SelectedHost = job.Host
	if database != nil {
		payloads, err := db.ListJobPayloads(database, job.ID)
		if err != nil {
			return runSubmissionReceipt{}, err
		}
		for _, payload := range payloads {
			receipt.Payloads = append(receipt.Payloads, runPayloadReceipt{Name: payload.Name, SizeBytes: payload.SizeBytes, SHA256: payload.SHA256})
		}
	}
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
	return receipt, nil
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
