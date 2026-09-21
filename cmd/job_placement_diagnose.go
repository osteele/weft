package cmd

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

const diagnoseDefaultMinSurvival = 0.4

type livePlacementDiagnoser struct {
	database *sql.DB
	once     sync.Once
	cfg      *config.Config
	clients  []cloud.Client
	r2Client *r2.Client
	err      error
}

func newLivePlacementDiagnoser(database *sql.DB) *livePlacementDiagnoser {
	return &livePlacementDiagnoser{database: database}
}

func (d *livePlacementDiagnoser) initialize() {
	d.once.Do(func() {
		d.cfg, d.err = config.Load()
		if d.err != nil {
			d.err = fmt.Errorf("load config: %w", d.err)
			return
		}
		d.clients, d.err = buildCloudClients(d.cfg)
		if d.err != nil {
			return
		}
		d.r2Client, d.err = buildR2Client(d.cfg)
		if d.err != nil {
			d.err = fmt.Errorf("initialize disk estimator: %w", d.err)
		}
	})
}

func (d *livePlacementDiagnoser) diagnose(job *db.Job) string {
	if !jobNeedsLivePlacementDiagnosis(job) {
		return ""
	}
	d.initialize()
	if d.err != nil {
		return "Placement analysis: unavailable: " + campaign.SanitizeBlockedReason(d.err.Error())
	}
	groups := campaign.PrepareGroupsWithConfig([]*db.Job{job}, d.database, d.cfg, "", d.r2Client)
	if len(groups) == 0 {
		return "Placement analysis: unavailable: could not derive this job's placement requirements"
	}
	diagnosis := campaign.DiagnosePlacement(
		d.clients,
		groups[0],
		buildSurvivalModel(d.database),
		campaign.PlacementDiagnosisOptions{
			MinReliability: d.cfg.CampaignReliability(),
			MinSurvival:    diagnoseDefaultMinSurvival,
		},
	)
	return formatLivePlacementDiagnosis(diagnosis)
}

func jobNeedsLivePlacementDiagnosis(job *db.Job) bool {
	if job == nil || job.EffectiveStatus() != db.StatusQueued || job.TargetKind() != db.JobTargetUnplaced {
		return false
	}
	return !job.UsesInventoryPlacement()
}

func formatLivePlacementDiagnosis(diagnosis campaign.PlacementDiagnosis) string {
	var b strings.Builder
	b.WriteString("Placement analysis (live market):\n")
	b.WriteString("  Exact: ")
	b.WriteString(formatPlacementProbe(diagnosis.Exact))
	b.WriteByte('\n')
	if diagnosis.Exact.Offer != nil {
		if note := formatAdmissionNote(diagnosis.Exact); note != "" {
			b.WriteString(note)
			b.WriteByte('\n')
		}
		b.WriteString("  The recorded blocker may clear on the next autopilot pass.\n")
	} else if len(diagnosis.Counterfactuals) == 0 {
		if len(diagnosis.TestedConstraints) > 0 {
			fmt.Fprintf(&b, "  Counterfactuals: no one-factor relaxation exposed a viable or additional candidate.\n")
		}
	} else {
		b.WriteString("  Counterfactuals:\n")
		for _, counterfactual := range diagnosis.Counterfactuals {
			fmt.Fprintf(&b, "    - %s: %s -> %s\n",
				counterfactual.Constraint,
				counterfactual.Relaxation,
				formatCounterfactualProbe(diagnosis.Exact, counterfactual.Result),
			)
		}
	}
	if len(diagnosis.TestedConstraints) > 0 {
		gpu := diagnosis.GPURequest
		if gpu == "" {
			gpu = "the requested GPU class"
		}
		fmt.Fprintf(&b, "  Scope: kept %s fixed; tested %s.\n", gpu, strings.Join(diagnosis.TestedConstraints, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatPlacementProbe(probe campaign.PlacementProbe) string {
	if probe.Err != nil {
		return campaign.SanitizeBlockedReason(probe.Err.Error())
	}
	if probe.Offer != nil {
		return "now finds " + formatPlacementOffer(*probe.Offer, probe.Survival)
	}
	if strings.TrimSpace(probe.Detail) != "" {
		return campaign.SanitizeBlockedReason(probe.Detail)
	}
	return "no viable offer"
}

// formatAdmissionNote states the constraint check that admitted a quoted
// offer. Quoting an offer without the admitting check reads as misrouting —
// a GPU offer quoted as the exact match for a CPU-only job made a reporter
// check whether they had mis-submitted (wb163). The searched-constraint list
// is the same predicate list the zero-offers branch reports, so the admitted
// and rejected directions describe the same filter chain.
func formatAdmissionNote(probe campaign.PlacementProbe) string {
	var parts []string
	if probe.SearchConstraints != "" {
		parts = append(parts, "it satisfies the searched constraints ("+probe.SearchConstraints+")")
	}
	if probe.RawCount > 0 && probe.KeptCount > 0 {
		parts = append(parts, fmt.Sprintf("%s found, %d passed all filters and it ranked first",
			offerNoun(probe.RawCount), probe.KeptCount))
	}
	if len(parts) == 0 {
		return ""
	}
	return "  Admitted because " + strings.Join(parts, "; ") + "."
}

func formatCounterfactualProbe(exact, probe campaign.PlacementProbe) string {
	if probe.Offer != nil {
		return "would admit " + formatPlacementOffer(*probe.Offer, probe.Survival)
	}
	additional := probe.RawCount - exact.RawCount
	if additional < 0 {
		additional = 0
	}
	return fmt.Sprintf("would expose %s additional; still %s", offerNoun(additional), campaign.SanitizeBlockedReason(probe.Detail))
}

func formatPlacementOffer(offer cloud.Offer, survival float64) string {
	provider := offer.Provider.DisplayName()
	if provider == "" {
		provider = string(offer.Provider)
	}
	parts := []string{strings.TrimSpace(provider + " " + offer.GPUName)}
	if offer.GPUMemGB > 0 {
		parts = append(parts, fmt.Sprintf("%.0fGB", offer.GPUMemGB))
	}
	if offer.CostPerHour > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f/hr", offer.CostPerHour))
	}
	if survival > 0 {
		parts = append(parts, fmt.Sprintf("predicted survival %.0f%%", survival*100))
	}
	return strings.Join(parts, ", ")
}

func offerNoun(count int) string {
	if count == 1 {
		return "1 class-matching candidate"
	}
	return fmt.Sprintf("%d class-matching candidates", count)
}
