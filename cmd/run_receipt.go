package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

const runReceiptAPIVersion = "weft.run.receipt.v1"

type runSubmissionReceipt struct {
	APIVersion          string `json:"api_version"`
	JobID               string `json:"job_id,omitempty"`
	PlacementDecision   string `json:"placement_decision"`
	SelectedHost        string `json:"selected_host,omitempty"`
	SourcePin           string `json:"source_pin,omitempty"`
	AcceptedImmediately bool   `json:"accepted_immediately"`
	Deduplicated        bool   `json:"deduplicated,omitempty"`
	IdempotencyKey      string `json:"idempotency_key,omitempty"`
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
	}
	return receipt
}

func normalizeRunCapabilities(agent string, capabilities []string) ([]string, error) {
	if name := strings.ToLower(strings.TrimSpace(agent)); name != "" {
		name = strings.TrimPrefix(name, "agent:")
		capabilities = append(capabilities, "agent:"+name)
	}
	seen := make(map[string]struct{}, len(capabilities))
	result := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" || strings.ContainsAny(capability, "\r\n\t ") {
			return nil, fmt.Errorf("invalid empty or whitespace-containing host capability %q", capability)
		}
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		result = append(result, capability)
	}
	return result, nil
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
