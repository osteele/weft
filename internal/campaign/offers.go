package campaign

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/placement"
)

// parseCUDAVersionFloat converts a CUDA major.minor string (e.g. "12.4") to
// float64 for comparison with placement.MinCUDAForGPU values.
func parseCUDAVersionFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// GroupOffer pairs an instance group with its best cloud offer.
type GroupOffer struct {
	Group          InstanceGroup
	Offer          *cloud.Offer // nil if no offers found
	SurvivalProb   float64      // 0 if no survival model available
	RejectedGroups []bidding.RejectedGroup
	Err            error
}

func offerConstraintsForGroup(group InstanceGroup) cloud.OfferConstraints {
	c := cloud.OfferConstraints{
		GPUClass:       group.GPUClass,
		MinGPUMemGB:    group.GPUMemGB,
		MinDiskGB:      group.DiskGB,
		MinReliability: cloud.DefaultMinReliability,
		// MaxGPUMemGB is intentionally NOT passed to the search filter.
		// It signals "no performance advantage above this tier" and is used
		// by BestOffer to cap effective DLPerf, not to exclude offers.
	}
	if group.HasComputeIntensiveJob() {
		c.MinCPUCoresEffective = intFromEnvOrDefault("WEFT_COMPUTE_CPU_CORES", 16)
	}
	return c
}

func intFromEnvOrDefault(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

// filterOffersByCUDACompat removes offers whose GPU requires a newer CUDA
// toolkit than the Docker image provides. Returns the compatible offers and
// the count of filtered offers (for logging).
func filterOffersByCUDACompat(offers []cloud.Offer, image string) ([]cloud.Offer, int) {
	if image == "" {
		image = cloud.DefaultImage
	}
	cudaVer, _, ok := parseCUDAImage(image)
	if !ok {
		return offers, 0
	}
	imageCUDA := parseCUDAVersionFloat(cudaVer)
	if imageCUDA == 0 {
		return offers, 0
	}

	compatible := make([]cloud.Offer, 0, len(offers))
	filtered := 0
	for _, o := range offers {
		minCUDA := placement.MinCUDAForGPU(o.GPUName)
		if minCUDA > 0 && minCUDA > imageCUDA {
			filtered++
			continue
		}
		compatible = append(compatible, o)
	}
	if filtered > 0 {
		slog.Debug("filtered offers by CUDA compatibility",
			"image_cuda", imageCUDA, "filtered", filtered, "remaining", len(compatible))
	}
	return compatible, filtered
}

// rankOffer selects the best offer from a slice and returns a GroupOffer.
func rankOffer(group InstanceGroup, offers []cloud.Offer, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupOverhead bidding.OfferSetupFunc, strategy bidding.SelectionStrategy, minSurvival float64) GroupOffer {
	result := GroupOffer{Group: group}
	if len(offers) == 0 {
		return result
	}
	offers, _ = filterOffersByCUDACompat(offers, group.Image)
	if len(offers) == 0 {
		return result
	}
	filtered, rejected := bidding.FilterOffersBySurvival(survivalModel, offers, minSurvival)
	result.RejectedGroups = rejected
	if len(filtered) == 0 {
		return result
	}
	_, best := bidding.BestOffer(survivalModel, filtered, jobDurationHrs, setupOverhead, strategy, group.MaxGPUMemGB)
	result.Offer = &best
	if survivalModel != nil {
		result.SurvivalProb = survivalModel.OfferSurvival(best)
	}
	return result
}

// SearchBestOfferForGroup searches cloud providers for the best offer matching a
// group's requirements, optionally excluding previously failed offer IDs.
func SearchBestOfferForGroup(
	clients []cloud.Client,
	group InstanceGroup,
	survivalModel *bidding.SurvivalModel,
	jobDurationHrs float64,
	setupOverhead bidding.OfferSetupFunc,
	excludeOfferIDs map[string]struct{},
	strategy bidding.SelectionStrategy,
	minSurvival float64,
) GroupOffer {
	offers, err := cloud.SearchAllProviders(clients, offerConstraintsForGroup(group))
	if err != nil {
		return GroupOffer{Group: group, Err: err}
	}

	if len(excludeOfferIDs) > 0 {
		filtered := offers[:0]
		for _, offer := range offers {
			if _, excluded := excludeOfferIDs[offer.Key()]; excluded {
				continue
			}
			filtered = append(filtered, offer)
		}
		offers = filtered
	}

	return rankOffer(group, offers, survivalModel, jobDurationHrs, setupOverhead, strategy, minSurvival)
}

// GroupRawOffers pairs an instance group with all available cloud offers (unranked).
type GroupRawOffers struct {
	Group  InstanceGroup
	Offers []cloud.Offer
	Err    error
}

// constraintKey returns a string key for deduplicating cloud searches.
// Groups with identical constraints produce identical offers.
func constraintKey(c cloud.OfferConstraints) string {
	return fmt.Sprintf("%s/%d/%d/%d/%.2f/%d",
		c.GPUClass, c.MinGPUMemGB, c.MaxGPUMemGB, c.MinDiskGB,
		c.MinReliability, c.MinCPUCoresEffective)
}

// FetchGroupRawOffers searches cloud providers for all offers per group, in parallel.
// Groups with identical constraints share a single search to avoid redundant API calls.
// Returns unranked offers suitable for caching and later ranking by strategy.
func FetchGroupRawOffers(clients []cloud.Client, groups []InstanceGroup) []GroupRawOffers {
	// Build constraint keys and identify unique searches
	keys := make([]string, len(groups))
	uniqueConstraints := make(map[string]cloud.OfferConstraints)
	for i, g := range groups {
		c := offerConstraintsForGroup(g)
		k := constraintKey(c)
		keys[i] = k
		uniqueConstraints[k] = c
	}

	// Search unique constraints in parallel
	type searchResult struct {
		key    string
		offers []cloud.Offer
		err    error
	}
	ch := make(chan searchResult, len(uniqueConstraints))
	for k, c := range uniqueConstraints {
		go func(key string, constraints cloud.OfferConstraints) {
			offers, err := cloud.SearchAllProviders(clients, constraints)
			ch <- searchResult{key, offers, err}
		}(k, c)
	}

	offersByKey := make(map[string]searchResult, len(uniqueConstraints))
	for range uniqueConstraints {
		r := <-ch
		offersByKey[r.key] = r
	}

	// Map results back to groups
	results := make([]GroupRawOffers, len(groups))
	for i, g := range groups {
		r := offersByKey[keys[i]]
		results[i] = GroupRawOffers{Group: g, Offers: r.offers, Err: r.err}
	}
	return results
}

// SetupOverheadFactory builds per-group OfferSetupFuncs. If nil, a constant
// 0.5h fallback is used for all groups.
type SetupOverheadFactory func(group InstanceGroup) bidding.OfferSetupFunc

// RankGroupOffers selects the best offer per group from cached raw offers using
// the given strategy. The setupFactory creates per-group setup overhead
// functions that account for datacenter download speed and group-specific
// download sizes. If nil, a constant 0.5h fallback is used.
func RankGroupOffers(raw []GroupRawOffers, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minSurvival float64) []GroupOffer {
	results := make([]GroupOffer, len(raw))
	for i, r := range raw {
		if r.Err != nil {
			results[i] = GroupOffer{Group: r.Group, Err: r.Err}
			continue
		}
		setupOverhead := bidding.ConstantSetup(0.5)
		if setupFactory != nil {
			setupOverhead = setupFactory(r.Group)
		}
		results[i] = rankOffer(r.Group, r.Offers, survivalModel, jobDurationHrs, setupOverhead, strategy, minSurvival)
	}
	return results
}

// MedianDLPerfFromRawOffers computes the median DLPerf across all raw offers.
// Returns 0 if no offers have DLPerf data.
func MedianDLPerfFromRawOffers(rawOffers []GroupRawOffers) float64 {
	var all []cloud.Offer
	for _, r := range rawOffers {
		all = append(all, r.Offers...)
	}
	return medianDLPerf(all)
}

// MedianDLPerfFromGroupOffers computes the median DLPerf from ranked group offers.
// Returns 0 if no selected offers have DLPerf data.
func MedianDLPerfFromGroupOffers(offers []GroupOffer) float64 {
	var all []cloud.Offer
	for _, o := range offers {
		if o.Offer != nil {
			all = append(all, *o.Offer)
		}
	}
	return medianDLPerf(all)
}

func medianDLPerf(offers []cloud.Offer) float64 {
	hasPerf := false
	for _, o := range offers {
		if o.DLPerf > 0 {
			hasPerf = true
			break
		}
	}
	if !hasPerf {
		return 0
	}
	return bidding.MedianOfferDLPerf(offers)
}

// FetchGroupOffers searches cloud providers for the best offer per group, in parallel.
// When survivalModel is non-nil, selects the offer with lowest expected cost (including
// retry risk from instance failure). Otherwise falls back to cheapest offer.
func FetchGroupOffers(clients []cloud.Client, groups []InstanceGroup, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minSurvival float64) []GroupOffer {
	raw := FetchGroupRawOffers(clients, groups)
	return RankGroupOffers(raw, survivalModel, jobDurationHrs, setupFactory, strategy, minSurvival)
}

// GroupingCandidate holds a candidate grouping of jobs with its raw offers.
// The outer search generates multiple candidates (split, merged, reuse) and
// scores each per strategy to find the best grouping.
type GroupingCandidate struct {
	Label  string // e.g. "split", "merged", "reuse"
	Groups []InstanceGroup
	Raw    []GroupRawOffers
}

// FetchCandidateGroupings generates candidate groupings (split, merged,
// parallel) and fetches raw offers for all in parallel. The split groups are
// the input; merged and parallel groups are derived variants.
//
// The "parallel" candidate splits multi-job groups into one-job-per-group,
// using each job's individual GPU memory requirements. This allows the scoring
// function to evaluate running jobs concurrently on separate instances, each
// sized to the individual job's needs.
func FetchCandidateGroupings(clients []cloud.Client, splitGroups []InstanceGroup) []GroupingCandidate {
	mergedGroups := MergeCompatibleGroups(splitGroups)
	parallelGroups := SplitToParallel(splitGroups)

	// Determine which candidates are distinct
	hasMerged := len(mergedGroups) != len(splitGroups)
	hasParallel := len(parallelGroups) != len(splitGroups)

	if !hasMerged && !hasParallel {
		raw := FetchGroupRawOffers(clients, splitGroups)
		return []GroupingCandidate{
			{Label: "split", Groups: splitGroups, Raw: raw},
		}
	}

	// Build candidate list and fetch offers in parallel
	type result struct {
		idx int
		raw []GroupRawOffers
	}

	var candidates []GroupingCandidate
	candidates = append(candidates, GroupingCandidate{Label: "split", Groups: splitGroups})
	if hasMerged {
		candidates = append(candidates, GroupingCandidate{Label: "merged", Groups: mergedGroups})
	}
	if hasParallel {
		candidates = append(candidates, GroupingCandidate{Label: "parallel", Groups: parallelGroups})
	}

	ch := make(chan result, len(candidates))
	for i, cand := range candidates {
		go func(idx int, groups []InstanceGroup) {
			ch <- result{idx, FetchGroupRawOffers(clients, groups)}
		}(i, cand.Groups)
	}
	for range candidates {
		r := <-ch
		candidates[r.idx].Raw = r.raw
	}
	return candidates
}

// ScoreGrouping evaluates a candidate grouping for a strategy using the unified
// weighted metric. Groups run in parallel, so time = max(per-group time) and
// cost = sum(per-group cost). Returns +Inf if any group has no valid offer.
func ScoreGrouping(groupOffers []GroupOffer, strategy bidding.SelectionStrategy) float64 {
	w := strategy.Weights()
	var totalCost float64
	var maxTime float64

	for _, go_ := range groupOffers {
		if go_.Offer == nil {
			return math.Inf(1)
		}
		// Use per-job cost as cost factor, wallclock as time factor.
		// For groups with multiple jobs, both cost and time scale with job count
		// (jobs run sequentially on one instance).
		numJobs := float64(len(go_.Group.Jobs))
		if numJobs < 1 {
			numJobs = 1
		}

		surv := go_.SurvivalProb
		if surv <= 0 {
			surv = 1.0 // no survival model
		}

		// Approximate: 1 hr per job, 0.5 hr setup
		setupHrs := 0.5
		runHrs := numJobs // 1 hr per job as baseline

		cost := bidding.ExpectedCost(go_.Offer.CostPerHour, runHrs, setupHrs, surv)
		wallclock := bidding.ExpectedWallclockTime(runHrs, setupHrs, surv)
		if strategy == bidding.StrategyFastest {
			wallclock = runHrs + setupHrs
		}

		totalCost += cost
		if wallclock > maxTime {
			maxTime = wallclock
		}
	}

	return w.Cost*totalCost + w.Time*maxTime
}

// BuildReuseCandidate constructs a candidate grouping that assigns jobs to
// existing reusable instances. Each compatible job is matched to the first
// reusable instance that can accept it (GPU, memory, disk). Unmatched jobs
// are excluded (they'll need new instances from other candidates).
//
// The returned candidate has synthetic GroupRawOffers with a single offer per
// group — the existing instance's specs at zero incremental cost (already rented).
func BuildReuseCandidate(jobs []*db.Job, instances []InstanceCapacity) *GroupingCandidate {
	if len(instances) == 0 || len(jobs) == 0 {
		return nil
	}

	// Map instance → assigned jobs
	type assignment struct {
		cap  InstanceCapacity
		jobs []*db.Job
	}
	assignments := make(map[int64]*assignment)
	for _, cap := range instances {
		assignments[cap.Instance.ID] = &assignment{cap: cap}
	}

	// Greedily assign jobs to first compatible instance
	var unmatched int
	for _, job := range jobs {
		matched := false
		for _, cap := range instances {
			if ok, _ := MatchJobToInstance(job, cap); ok {
				assignments[cap.Instance.ID].jobs = append(assignments[cap.Instance.ID].jobs, job)
				matched = true
				break
			}
		}
		if !matched {
			unmatched++
		}
	}

	// If no jobs matched any instance, no reuse candidate
	if unmatched == len(jobs) {
		return nil
	}

	// Build groups and synthetic offers from matched assignments
	var groups []InstanceGroup
	var raw []GroupRawOffers
	for _, a := range assignments {
		if len(a.jobs) == 0 {
			continue
		}
		inst := a.cap.Instance
		group := InstanceGroup{
			GPUClass: inst.GPUClass,
			GPUMemGB: inst.GPUMemGB,
			DiskGB:   inst.DiskGB,
			Jobs:     a.jobs,
		}
		// Zero cost: instance is already rented
		syntheticOffer := cloud.Offer{
			ProviderID:  inst.ProviderInstanceID,
			Provider:    cloud.Provider(inst.Provider),
			GPUName:     inst.ResolvedGPUName,
			GPUMemGB:    float64(inst.GPUMemGB),
			CostPerHour: 0, // already paying for it
			DLPerf:      inst.DLPerf,
			Reliability: inst.Reliability,
		}
		groups = append(groups, group)
		raw = append(raw, GroupRawOffers{
			Group:  group,
			Offers: []cloud.Offer{syntheticOffer},
		})
	}

	if len(groups) == 0 {
		return nil
	}

	return &GroupingCandidate{
		Label:  "reuse",
		Groups: groups,
		Raw:    raw,
	}
}

// CandidateResult holds the winning candidate's grouping and offers for a strategy.
type CandidateResult struct {
	CandidateIdx int
	Label        string
	Groups       []InstanceGroup
	Offers       []GroupOffer
}

// BestCandidateForStrategy selects the candidate grouping with the lowest score
// for the given strategy from pre-ranked offers.
func BestCandidateForStrategy(
	candidates []GroupingCandidate,
	survivalModel *bidding.SurvivalModel,
	jobDurationHrs float64,
	setupFactory SetupOverheadFactory,
	strategy bidding.SelectionStrategy,
	minSurvival float64,
) CandidateResult {
	bestScore := math.Inf(1)
	var best CandidateResult

	for i, cand := range candidates {
		offers := RankGroupOffers(cand.Raw, survivalModel, jobDurationHrs, setupFactory, strategy, minSurvival)
		score := ScoreGrouping(offers, strategy)
		if score < bestScore {
			bestScore = score
			best = CandidateResult{
				CandidateIdx: i,
				Label:        cand.Label,
				Groups:       cand.Groups,
				Offers:       offers,
			}
		}
	}
	return best
}

// MapOffersToSplitGroups maps a winning candidate's offers back to the original
// split groups for display. Each split group is assigned the offer from the
// candidate group that contains its jobs. Split groups with no matching
// candidate group get a nil-offer GroupOffer.
func MapOffersToSplitGroups(splitGroups []InstanceGroup, result CandidateResult) []GroupOffer {
	// Build job ID → candidate group index mapping
	jobToCandidate := make(map[int64]int)
	for ci, cg := range result.Groups {
		for _, job := range cg.Jobs {
			jobToCandidate[job.ID] = ci
		}
	}

	mapped := make([]GroupOffer, len(splitGroups))
	for si, sg := range splitGroups {
		mapped[si] = GroupOffer{Group: sg}
		if len(sg.Jobs) == 0 {
			continue
		}
		// Use the first job's candidate group (all jobs in a split group
		// should map to the same candidate group, since merging only
		// combines whole groups)
		if ci, ok := jobToCandidate[sg.Jobs[0].ID]; ok && ci < len(result.Offers) {
			mapped[si].Offer = result.Offers[ci].Offer
			mapped[si].SurvivalProb = result.Offers[ci].SurvivalProb
			mapped[si].RejectedGroups = result.Offers[ci].RejectedGroups
			mapped[si].Err = result.Offers[ci].Err
		}
	}
	return mapped
}
