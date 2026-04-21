package terminal

import (
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// formatRentalInstanceLabel returns the wi<launch-id> instance label with a
// 2-letter provider prefix when the job's provider tag is known (e.g.
// "va:wi1210"). Returns bare "wi<launch-id>" when the provider is absent.
// Callers must have already verified that job.LaunchID != nil.
func formatRentalInstanceLabel(job *db.Job) string {
	label := ids.FormatInstanceID(*job.LaunchID)
	if code := cloud.Provider(job.ProviderName()).ShortCode(); code != "" {
		return code + ":" + label
	}
	return label
}
