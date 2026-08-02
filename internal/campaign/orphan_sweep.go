package campaign

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// weftLabelPrefix is the label prefix used for Vast.ai instances.
const weftLabelPrefix = "weft/"

// weftNamePrefix is the name prefix used for RunPod instances.
const weftNamePrefix = "weft-"

// orphanSweepInterval is the minimum time between orphan sweeps.
const orphanSweepInterval = 5 * time.Minute

var (
	lastOrphanSweepMu sync.Mutex
	lastOrphanSweep   time.Time
)

type cleanupInventoryClient interface {
	ListInstancesForCleanup() ([]cloud.Instance, error)
}

// SweepOrphanedInstances lists all instances from each provider, filters to
// weft-labeled ones, cross-references with the local DB, and destroys any
// whose launch row is terminal or whose provider ID is unknown to weft.
// The campaign label is treated as ownership only — campaign status is
// never used as a kill switch (a long-lived job stuck on a "failed"
// campaign should keep running until its launch row terminates on its
// own).
// Only instances with the weft label/name prefix are considered — unlabeled
// instances are never touched.
func SweepOrphanedInstances(database *sql.DB, clients []cloud.Client) (destroyed int, err error) {
	for _, client := range clients {
		instances, listErr := listInstancesForOrphanSweep(client)
		if listErr != nil {
			slog.Warn("ListAllInstances failed", "component", "orphan-sweep", "provider", client.Provider(), "error", listErr)
			continue
		}

		for _, inst := range instances {
			if !isWeftInstance(inst.Label) {
				continue
			}
			if !needsProviderDestroy(&inst) {
				continue
			}

			shouldDestroy, reason := shouldDestroyWeftInstance(database, inst.Label, inst.ProviderID)
			if !shouldDestroy {
				continue
			}

			slog.Info("destroying orphaned instance", "component", "orphan-sweep", "provider", client.Provider(), "provider_id", inst.ProviderID, "label", inst.Label, "reason", reason)
			if destroyErr := client.DestroyInstance(inst.ProviderID); destroyErr != nil {
				slog.Warn("failed to destroy orphaned instance", "component", "orphan-sweep", "provider_id", inst.ProviderID, "error", destroyErr)
				continue
			}
			destroyed++
		}
	}
	return destroyed, nil
}

func listInstancesForOrphanSweep(client cloud.Client) ([]cloud.Instance, error) {
	if cleanupClient, ok := client.(cleanupInventoryClient); ok {
		return cleanupClient.ListInstancesForCleanup()
	}
	return client.ListAllInstances()
}

// shouldDestroyWeftInstance returns true when a weft-labeled provider
// instance should be reaped. The decision depends only on the launch row
// in our DB — never on the parent campaign's status. Campaign-scoped
// labels (weft/c42) are treated identically to instance labels (weft/i42)
// because the orphan sweep's job is to kill billable provider instances
// whose launch is gone or terminal, not to enforce a "campaign failed →
// kill all live members" policy. A long-lived rental whose owning
// campaign has been stamped failed (often days or weeks ago) keeps
// running until its own launch row terminates.
func shouldDestroyWeftInstance(database *sql.DB, label string, providerID string) (bool, string) {
	launch, err := db.GetLaunchByProviderID(database, providerID)
	if err != nil {
		slog.Warn("failed to look up launch by provider ID", "component", "orphan-sweep", "provider_id", providerID, "error", err)
		return false, ""
	}
	if launch == nil {
		return shouldDestroyUnrecordedWeftInstance(database, label, providerID)
	}
	if launch.IsTerminal() {
		return true, fmt.Sprintf("launch %d is %s", launch.ID, launch.Status)
	}
	return false, ""
}

// shouldDestroyUnrecordedWeftInstance decides the fate of a weft-labeled
// provider instance whose provider ID matches no launch row: usually a
// crashed launcher's leak, but briefly also a healthy instance whose
// create has not yet recorded its provider ID. The label names the
// owning launch (weft/i<id>) or campaign (weft/c<id>); destroy only when
// no create that could own this instance is in flight, deferring
// otherwise to the next sweep. See status-sync.allium § ReconcilePass.
func shouldDestroyUnrecordedWeftInstance(database *sql.DB, label, providerID string) (bool, string) {
	if launchID, ok := extractLaunchID(label); ok {
		labeled, err := db.GetLaunch(database, launchID)
		if err != nil {
			slog.Warn("failed to look up labeled launch", "component", "orphan-sweep", "provider_id", providerID, "label", label, "error", err)
			return false, ""
		}
		if labeled == nil {
			return true, fmt.Sprintf("labeled launch %d not found in DB", launchID)
		}
		if labeled.IsTerminal() {
			return true, fmt.Sprintf("labeled launch %d is %s", launchID, labeled.Status)
		}
		if labeled.EffectiveProviderID() == "" {
			return false, "" // create in flight: the labeled launch has not recorded its provider ID yet
		}
		// The labeled launch is live and bound to a different provider
		// instance: this one is an abandoned duplicate from a create retry.
		return true, fmt.Sprintf("labeled launch %d is bound to provider instance %s", launchID, labeled.EffectiveProviderID())
	}
	if campaignID, ok := extractCampaignID(label); ok {
		inFlight, err := campaignHasCreateInFlight(database, campaignID)
		if err != nil {
			slog.Warn("failed to check campaign create-in-flight", "component", "orphan-sweep", "provider_id", providerID, "campaign", campaignID, "error", err)
			return false, ""
		}
		if inFlight {
			return false, ""
		}
	}
	return true, "weft-labeled instance not found in DB"
}

// campaignHasCreateInFlight reports whether the campaign has a launch that
// could still be mid-create: planned or launching with no provider ID
// recorded. Campaign labels don't identify which launch owns a provider
// instance, so any such launch defers the destroy for this sweep.
func campaignHasCreateInFlight(database *sql.DB, campaignID int64) (bool, error) {
	launches, err := db.GetCampaignInstances(database, campaignID)
	if err != nil {
		return false, err
	}
	for _, l := range launches {
		midCreate := l.Status == db.LaunchStatusPlanned || l.Status == db.LaunchStatusLaunching
		if midCreate && l.EffectiveProviderID() == "" {
			return true, nil
		}
	}
	return false, nil
}

// isWeftInstance returns true if the label indicates a weft-managed instance.
func isWeftInstance(label string) bool {
	return strings.HasPrefix(label, weftLabelPrefix) || strings.HasPrefix(label, weftNamePrefix)
}

// extractCampaignID parses a campaign ID from a weft label/name.
// Accepts "weft/c42" (Vast.ai label) or "weft-c42" (RunPod name).
func extractCampaignID(label string) (int64, bool) {
	return extractWeftLabelID(label, "c")
}

// launchProviderLabel is the provider-side label/name stamped on every
// weft instance at create time; extractLaunchID is its parser.
func launchProviderLabel(launchID int64) string {
	return fmt.Sprintf("weft/i%d", launchID)
}

// extractLaunchID parses a launch ID from a weft label/name.
// Accepts "weft/i42" (Vast.ai label) or "weft-i42" (RunPod name).
func extractLaunchID(label string) (int64, bool) {
	return extractWeftLabelID(label, "i")
}

func extractWeftLabelID(label, kind string) (int64, bool) {
	idStr, ok := strings.CutPrefix(label, weftLabelPrefix+kind)
	if !ok {
		if idStr, ok = strings.CutPrefix(label, weftNamePrefix+kind); !ok {
			return 0, false
		}
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// needsProviderDestroy returns true if the provider instance is still billable
// and should be destroyed. Unlike isProviderTerminal (used by the reconciler to
// mean "no longer active"), this treats "stopped" as needing destruction because
// providers like Vast.ai continue charging for disk on stopped instances.
func needsProviderDestroy(inst *cloud.Instance) bool {
	if inst == nil {
		return false // not found = already gone
	}
	switch inst.Status {
	case cloud.ProviderStatusDestroyed, cloud.ProviderStatusDead:
		return false
	}
	return true
}

// MaybeSweepOrphanedInstances runs SweepOrphanedInstances if at least
// orphanSweepInterval has elapsed since the last sweep.
func MaybeSweepOrphanedInstances(database *sql.DB, clients []cloud.Client) (int, error) {
	lastOrphanSweepMu.Lock()
	if time.Since(lastOrphanSweep) < orphanSweepInterval {
		lastOrphanSweepMu.Unlock()
		return 0, nil
	}
	lastOrphanSweep = time.Now()
	lastOrphanSweepMu.Unlock()

	return SweepOrphanedInstances(database, clients)
}

const stalePlannedLaunchAge = 24 * time.Hour

// SweepStalePlannedLaunches retires launch rows abandoned in 'planned'
// status before the provider instance was ever created. See the
// StalePlannedLaunchReap rule in specs/campaign-lifecycle.allium.
func SweepStalePlannedLaunches(database *sql.DB) (reaped int, err error) {
	cutoff := time.Now().Add(-stalePlannedLaunchAge).Unix()
	launches, err := db.ListStalePlannedLaunches(database, cutoff)
	if err != nil {
		return 0, fmt.Errorf("list stale planned launches: %w", err)
	}
	for _, l := range launches {
		detail := fmt.Sprintf("reaped: planned launch never created on provider (age %s)",
			time.Since(time.Unix(l.CreatedAt, 0)).Round(time.Hour))
		if err := db.UpdateLaunchStatus(database, l.ID,
			db.LaunchStatusCancelled, db.TerminationReasonCancelled, detail); err != nil {
			slog.Warn("failed to reap stale planned launch",
				"component", "planned-reap", "launch_id", l.ID, "error", err)
			continue
		}
		slog.Info("reaped stale planned launch",
			"component", "planned-reap",
			"launch_id", l.ID,
			"campaign_id", l.CampaignID,
			"created_at", time.Unix(l.CreatedAt, 0),
			"gpu_spec", l.GPUSpec)
		reaped++
	}
	return reaped, nil
}

const plannedReapInterval = 5 * time.Minute

var (
	lastPlannedReapMu sync.Mutex
	lastPlannedReap   time.Time
)

// MaybeSweepStalePlannedLaunches runs SweepStalePlannedLaunches if at least
// plannedReapInterval has elapsed since the last sweep.
func MaybeSweepStalePlannedLaunches(database *sql.DB) (int, error) {
	lastPlannedReapMu.Lock()
	if time.Since(lastPlannedReap) < plannedReapInterval {
		lastPlannedReapMu.Unlock()
		return 0, nil
	}
	lastPlannedReap = time.Now()
	lastPlannedReapMu.Unlock()

	return SweepStalePlannedLaunches(database)
}
