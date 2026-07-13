package orchestration

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

var autoPilotBuildCloudClients = BuildCloudClients

func buildAutoPlacementPlan(
	database *sql.DB,
	cfg *config.Config,
	jobs []*db.Job,
	reusable []campaign.InstanceCapacity,
) (campaign.AutoPlacementPlan, error) {
	return buildAutoPlacementPlanWithOptions(database, cfg, jobs, reusable, campaign.AutoPlannerOptions(cfg))
}

func buildAutoPlacementPlanWithOptions(
	database *sql.DB,
	cfg *config.Config,
	jobs []*db.Job,
	reusable []campaign.InstanceCapacity,
	options campaign.PlanOptions,
) (campaign.AutoPlacementPlan, error) {
	if cfg == nil {
		var err error
		cfg, err = config.Load()
		if err != nil {
			return campaign.AutoPlacementPlan{}, err
		}
	}
	clients, err := autoPilotBuildCloudClients(cfg)
	if err != nil {
		clients = nil
	}
	predCfg := buildPredictorConfig(cfg)
	overheadModel := buildOverheadModel(database)
	survivalModel := buildSurvivalModel(database)
	return campaign.BuildAutoPlacementPlanWithOptions(
		database,
		cfg,
		clients,
		jobs,
		reusable,
		&predCfg,
		overheadModel,
		survivalModel,
		0,
		options,
	)
}

type autoPilotOfferSnapshotEntry struct {
	raw       campaign.GroupRawOffers
	fetchedAt time.Time
}

var autoPilotOfferSnapshotCache = struct {
	sync.Mutex
	entries map[string]autoPilotOfferSnapshotEntry
}{
	entries: make(map[string]autoPilotOfferSnapshotEntry),
}

func buildAutoPilotCachedOfferOptions(database *sql.DB, cfg *config.Config, scopedJobs []*db.Job) campaign.PlanOptions {
	options := campaign.AutoPlannerOptions(cfg)
	raw := fetchAutoPilotOfferSnapshot(database, cfg, scopedJobs)
	if len(raw) == 0 {
		return options
	}
	options.RawOffers = raw
	options.CachedOffersOnly = true
	return options
}

func fetchAutoPilotOfferSnapshot(database *sql.DB, cfg *config.Config, scopedJobs []*db.Job) []campaign.GroupRawOffers {
	if database == nil || cfg == nil {
		return nil
	}
	groups := autoPilotOfferSnapshotGroups(database, cfg, scopedJobs)
	if len(groups) == 0 {
		return nil
	}
	minReliability := cfg.CampaignReliability()
	ttl := cfg.AutopilotOfferSnapshotTTL()
	if cached, ok := cachedAutoPilotRawOffers(groups, minReliability, ttl, true); ok {
		return cached
	}

	clients, err := autoPilotBuildCloudClients(cfg)
	if err != nil || len(clients) == 0 {
		return fallbackAutoPilotRawOffers(groups, minReliability, err)
	}
	raw := campaign.FetchGroupRawOffers(clients, groups, minReliability)
	now := time.Now()
	autoPilotOfferSnapshotCache.Lock()
	for _, groupRaw := range raw {
		if groupRaw.Err != nil {
			continue
		}
		key := campaign.GroupRawOfferCacheKey(groupRaw.Group, minReliability)
		autoPilotOfferSnapshotCache.entries[key] = autoPilotOfferSnapshotEntry{
			raw:       cloneGroupRawOffers(groupRaw),
			fetchedAt: now,
		}
	}
	autoPilotOfferSnapshotCache.Unlock()

	for i := range raw {
		if raw[i].Err == nil {
			continue
		}
		if cached, ok := cachedAutoPilotRawOffer(raw[i].Group, minReliability, false, ttl); ok {
			raw[i] = cached
		} else {
			raw[i].Err = fmt.Errorf("%w: %v", campaign.ErrOfferSnapshotUnavailable, raw[i].Err)
		}
	}
	return raw
}

func autoPilotOfferSnapshotGroups(database *sql.DB, cfg *config.Config, scopedJobs []*db.Job) []campaign.InstanceGroup {
	scoped := make(map[int64]struct{}, len(scopedJobs))
	for _, job := range scopedJobs {
		if job != nil {
			scoped[job.ID] = struct{}{}
		}
	}
	movingJobs, err := db.JobIDsWithOpenMoveOrPlacementIntents(database)
	if err != nil {
		return nil
	}
	unplacedJobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil
	}
	candidates := make([]*db.Job, 0, len(unplacedJobs))
	for _, job := range unplacedJobs {
		if job == nil || !job.IsUnplacedAwaitingPlacement() || job.HasTag(db.TagInventory) {
			continue
		}
		if _, moving := movingJobs[job.ID]; moving {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[job.ID]; !ok {
				continue
			}
		}
		candidates = append(candidates, job)
	}
	groups := campaign.GroupByAffinity(candidates, nil)
	groups = campaign.SplitGroupsByImage(database, groups)
	groups = campaign.ApplyImageMetadataRequirements(cfg, groups)
	campaign.ApplyBidLossEscalation(database, groups)
	groups = campaign.EstimateGroupDisks(groups, database, nil)
	return uniqueAutoPilotOfferSnapshotGroups(database, groups, cfg.CampaignReliability())
}

func uniqueAutoPilotOfferSnapshotGroups(database *sql.DB, groups []campaign.InstanceGroup, minReliability float64) []campaign.InstanceGroup {
	candidates := make([]campaign.InstanceGroup, 0, len(groups)*3)
	candidates = append(candidates, groups...)
	candidates = append(candidates, campaign.MergeCompatibleGroupsWithDisk(groups, campaign.GroupDiskEstimator(database))...)
	candidates = append(candidates, campaign.SplitToParallel(groups)...)

	seen := make(map[string]struct{}, len(candidates))
	out := make([]campaign.InstanceGroup, 0, len(candidates))
	for _, group := range candidates {
		key := campaign.GroupRawOfferCacheKey(group, minReliability)
		if strings.TrimSpace(key) == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, group)
	}
	return out
}

func cachedAutoPilotRawOffers(groups []campaign.InstanceGroup, minReliability float64, ttl time.Duration, requireFresh bool) ([]campaign.GroupRawOffers, bool) {
	raw := make([]campaign.GroupRawOffers, 0, len(groups))
	for _, group := range groups {
		cached, ok := cachedAutoPilotRawOffer(group, minReliability, requireFresh, ttl)
		if !ok {
			return nil, false
		}
		raw = append(raw, cached)
	}
	return raw, true
}

func cachedAutoPilotRawOffer(group campaign.InstanceGroup, minReliability float64, requireFresh bool, ttl time.Duration) (campaign.GroupRawOffers, bool) {
	key := campaign.GroupRawOfferCacheKey(group, minReliability)
	autoPilotOfferSnapshotCache.Lock()
	entry, ok := autoPilotOfferSnapshotCache.entries[key]
	autoPilotOfferSnapshotCache.Unlock()
	if !ok {
		return campaign.GroupRawOffers{}, false
	}
	if requireFresh && time.Since(entry.fetchedAt) >= ttl {
		return campaign.GroupRawOffers{}, false
	}
	raw := cloneGroupRawOffers(entry.raw)
	raw.Group = group
	return raw, true
}

func fallbackAutoPilotRawOffers(groups []campaign.InstanceGroup, minReliability float64, fetchErr error) []campaign.GroupRawOffers {
	raw := make([]campaign.GroupRawOffers, 0, len(groups))
	for _, group := range groups {
		if cached, ok := cachedAutoPilotRawOffer(group, minReliability, false, 0); ok {
			raw = append(raw, cached)
			continue
		}
		err := campaign.ErrOfferSnapshotUnavailable
		if fetchErr != nil {
			err = fmt.Errorf("%w: %v", err, fetchErr)
		}
		raw = append(raw, campaign.GroupRawOffers{Group: group, Err: err})
	}
	return raw
}

func cloneGroupRawOffers(raw campaign.GroupRawOffers) campaign.GroupRawOffers {
	raw.Offers = append([]cloud.Offer(nil), raw.Offers...)
	return raw
}

func mergeCachedOfferOptions(base campaign.PlanOptions, snapshot campaign.PlanOptions) campaign.PlanOptions {
	base.RawOffers = snapshot.RawOffers
	base.CachedOffersOnly = snapshot.CachedOffersOnly
	if base.OpportunityCostWeight <= 0 {
		base.OpportunityCostWeight = snapshot.OpportunityCostWeight
	}
	if len(base.InitialClaimedMachines) == 0 {
		base.InitialClaimedMachines = snapshot.InitialClaimedMachines
	}
	if len(base.MachineAffinity) == 0 {
		base.MachineAffinity = snapshot.MachineAffinity
	}
	return base
}
