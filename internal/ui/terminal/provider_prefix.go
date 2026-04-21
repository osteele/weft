package terminal

import (
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// providerShortPrefix returns a 2-letter tag for the given provider name, used
// as a compact prefix in the jobs-list HOST column, the grouped "launching"
// row suffix, and the selected-job detail line to disambiguate rental
// instances across providers. Returns "" for unknown or empty provider names.
func providerShortPrefix(provider string) string {
	switch provider {
	case string(cloud.ProviderVastai):
		return "va"
	case string(cloud.ProviderRunpod):
		return "rp"
	default:
		return ""
	}
}

// formatRentalInstanceLabel returns the wi<launch-id> instance label with a
// 2-letter provider prefix when the job's provider tag is known (e.g.
// "va:wi1210"). Returns bare "wi<launch-id>" when the provider is absent.
// Callers must have already verified that job.LaunchID != nil.
func formatRentalInstanceLabel(job *db.Job) string {
	label := ids.FormatInstanceID(*job.LaunchID)
	if prefix := providerShortPrefix(job.ProviderName()); prefix != "" {
		return prefix + ":" + label
	}
	return label
}
