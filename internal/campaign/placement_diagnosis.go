package campaign

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// PlacementDiagnosisOptions supplies the policy floors used by the placement
// pass whose result is being diagnosed.
type PlacementDiagnosisOptions struct {
	MinReliability float64
	MinSurvival    float64
}

// PlacementProbe is the result of one exact or counterfactual offer search.
type PlacementProbe struct {
	Offer          *cloud.Offer
	Survival       float64
	RawCount       int
	Detail         string
	ProviderErrors []string
	Err            error
}

// PlacementCounterfactual describes one constraint relaxation that changed
// the observed GPU-class-matching candidate set or made an offer viable.
type PlacementCounterfactual struct {
	Constraint string
	Relaxation string
	Result     PlacementProbe
}

// PlacementDiagnosis compares the exact placement request with bounded,
// one-factor-at-a-time probes. GPUClass is never relaxed: a diagnosis for an
// L4 request can inspect L4 candidates across providers, but cannot cite an
// H100 as a near miss.
type PlacementDiagnosis struct {
	GPURequest        string
	Exact             PlacementProbe
	Counterfactuals   []PlacementCounterfactual
	TestedConstraints []string
}

type placementProbeSpec struct {
	constraint  string
	relaxation  string
	group       InstanceGroup
	reliability float64
}

// DiagnosePlacement performs an exact offer search followed, when the exact
// request has no viable offer, by bounded GPU-class-scoped counterfactuals.
// Independent probes run concurrently because each one is a provider call.
func DiagnosePlacement(clients []cloud.Client, group InstanceGroup, survivalModel *bidding.SurvivalModel, opts PlacementDiagnosisOptions) PlacementDiagnosis {
	diagnosis := PlacementDiagnosis{GPURequest: strings.TrimSpace(group.GPUClass)}
	diagnosis.Exact = runPlacementProbe(clients, group, survivalModel, opts.MinReliability, opts.MinSurvival)
	if diagnosis.Exact.Offer != nil || (diagnosis.Exact.Err != nil && !errors.Is(diagnosis.Exact.Err, ErrMachineAffinityUnsatisfied)) {
		return diagnosis
	}

	specs := placementCounterfactualSpecs(group, opts)
	diagnosis.TestedConstraints = make([]string, 0, len(specs))
	for _, spec := range specs {
		diagnosis.TestedConstraints = append(diagnosis.TestedConstraints, spec.constraint)
	}

	type indexedResult struct {
		index int
		probe PlacementProbe
	}
	results := make(chan indexedResult, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(index int, candidate placementProbeSpec) {
			defer wg.Done()
			results <- indexedResult{
				index: index,
				probe: runPlacementProbe(clients, candidate.group, survivalModel, candidate.reliability, opts.MinSurvival),
			}
		}(i, spec)
	}
	wg.Wait()
	close(results)

	probes := make([]PlacementProbe, len(specs))
	for result := range results {
		probes[result.index] = result.probe
	}
	for i, probe := range probes {
		if placementProbeChangedCandidateSet(diagnosis.Exact, probe) {
			diagnosis.Counterfactuals = append(diagnosis.Counterfactuals, PlacementCounterfactual{
				Constraint: specs[i].constraint,
				Relaxation: specs[i].relaxation,
				Result:     probe,
			})
		}
	}
	return diagnosis
}

func runPlacementProbe(clients []cloud.Client, group InstanceGroup, survivalModel *bidding.SurvivalModel, minReliability, minSurvival float64) PlacementProbe {
	if len(clients) == 0 {
		return PlacementProbe{Err: fmt.Errorf("no cloud providers enabled")}
	}
	raw := FetchGroupRawOffers(clients, []InstanceGroup{group}, minReliability)
	if len(raw) == 0 {
		return PlacementProbe{Err: fmt.Errorf("offer search produced no result")}
	}
	if raw[0].Err != nil {
		return PlacementProbe{Err: raw[0].Err, ProviderErrors: raw[0].ProviderErrors}
	}
	ranked := RankGroupOffers(raw, survivalModel, 1, nil, bidding.StrategyCheap, minSurvival)
	if len(ranked) == 0 {
		return PlacementProbe{Err: fmt.Errorf("offer ranking produced no result")}
	}
	result := ranked[0]
	probe := PlacementProbe{
		Offer:          result.Offer,
		Survival:       result.SurvivalProb,
		RawCount:       result.FilterStats.RawCount,
		ProviderErrors: append([]string(nil), result.FilterStats.ProviderErrors...),
		Err:            result.Err,
	}
	if result.Err != nil {
		probe.Detail = result.Err.Error()
	} else if result.Offer != nil {
		probe.Detail = "viable offer found"
	} else {
		probe.Detail = result.FilterStats.NoOffersDetail(formatProviderSearchConstraints(offerConstraintsForGroup(group, minReliability)))
	}
	return probe
}

func placementProbeChangedCandidateSet(exact, probe PlacementProbe) bool {
	if probe.Err != nil {
		return false
	}
	if probe.Offer != nil && exact.Offer == nil {
		return true
	}
	return probe.RawCount > exact.RawCount
}

func placementCounterfactualSpecs(group InstanceGroup, opts PlacementDiagnosisOptions) []placementProbeSpec {
	var specs []placementProbeSpec
	add := func(constraint, relaxation string, candidate InstanceGroup, reliability float64) {
		// A hard guard, not a convention: no counterfactual may widen GPU
		// identity. Memory floors may be tested while the GPU class remains.
		candidate.GPUClass = group.GPUClass
		specs = append(specs, placementProbeSpec{
			constraint:  constraint,
			relaxation:  relaxation,
			group:       candidate,
			reliability: reliability,
		})
	}
	if strings.TrimSpace(group.Provider) != "" {
		candidate := group
		candidate.Provider = ""
		add("provider pin", "allow any enabled provider", candidate, opts.MinReliability)
	}
	if strings.TrimSpace(group.RunpodCloudType) != "" {
		candidate := group
		candidate.RunpodCloudType = ""
		add("RunPod cloud type", "allow either RunPod cloud type", candidate, opts.MinReliability)
	}
	if group.GPUMemGB > 0 {
		candidate := group
		candidate.GPUMemGB = 0
		add("GPU memory floor", fmt.Sprintf("ignore the %dGB GPU memory floor", group.GPUMemGB), candidate, opts.MinReliability)
	}
	if group.DiskGB > 0 {
		candidate := group
		candidate.DiskGB = 0
		add("disk floor", fmt.Sprintf("ignore the %dGB disk floor", group.DiskGB), candidate, opts.MinReliability)
	}
	if group.CPUCores > 0 || group.HasComputeIntensiveJob() {
		candidate := clonePlacementGroupJobs(group)
		candidate.CPUCores = 0
		removePlacementTag(candidate.Jobs, db.TagCPUIntensive)
		add("CPU floor", "ignore the CPU-core floor, including cpu-intensive", candidate, opts.MinReliability)
	}
	if group.CPUMemGB > 0 {
		candidate := group
		candidate.CPUMemGB = 0
		add("host RAM floor", fmt.Sprintf("ignore the %dGB host RAM floor", group.CPUMemGB), candidate, opts.MinReliability)
	}
	if opts.MinReliability > 0 {
		add("provider reliability floor", fmt.Sprintf("ignore the %.0f%% provider reliability floor", opts.MinReliability*100), group, 0)
	}
	if group.MinDriverVersion > 0 {
		candidate := group
		candidate.MinDriverVersion = 0
		add("driver floor", fmt.Sprintf("ignore the NVIDIA driver >=%d floor", group.MinDriverVersion), candidate, opts.MinReliability)
	}
	if strings.TrimSpace(group.MinCUDAVersion) != "" {
		candidate := group
		candidate.MinCUDAVersion = ""
		add("CUDA floor", "ignore the CUDA >="+strings.TrimSpace(group.MinCUDAVersion)+" floor", candidate, opts.MinReliability)
	}
	if group.MinComputeCap != "" || group.MaxComputeCap != "" {
		candidate := group
		candidate.MinComputeCap = ""
		candidate.MaxComputeCap = ""
		add("torch architecture bounds", "ignore torch compute-capability bounds", candidate, opts.MinReliability)
	}
	if capCents, _ := db.RequestedMaxHourlyRateCentsForJobs(group.Jobs); capCents > 0 {
		candidate := clonePlacementGroupJobs(group)
		for _, job := range candidate.Jobs {
			if job.CLIResourceOverrides != nil {
				job.CLIResourceOverrides.MaxHourlyRateCents = nil
			}
		}
		add("hourly-rate cap", fmt.Sprintf("ignore the $%.2f/hr cap", float64(capCents)/100), candidate, opts.MinReliability)
	}
	if floor := db.RequestedMinSurvivalForJobs(group.Jobs, opts.MinSurvival); floor > 0 {
		candidate := clonePlacementGroupJobs(group)
		zero := 0.0
		for _, job := range candidate.Jobs {
			ensurePlacementOverrides(job).MinSurvival = &zero
		}
		add("survival floor", fmt.Sprintf("ignore the %.0f%% predicted-survival floor", floor*100), candidate, opts.MinReliability)
	}
	if len(db.RequestedMachineAffinityForJobs(group.Jobs)) > 0 {
		candidate := clonePlacementGroupJobs(group)
		for _, job := range candidate.Jobs {
			if job.CLIResourceOverrides != nil {
				job.CLIResourceOverrides.MachineAffinity = nil
			}
		}
		add("machine pin", "ignore provider-machine affinity", candidate, opts.MinReliability)
	}
	if group.HasPreemptibleJob() {
		candidate := clonePlacementGroupJobs(group)
		candidate.Preemptible = false
		removePlacementTag(candidate.Jobs, db.TagInterruptible)
		add("interruptible-only", "allow on-demand offers", candidate, opts.MinReliability)
	}
	return specs
}

func clonePlacementGroupJobs(group InstanceGroup) InstanceGroup {
	clone := group
	clone.Jobs = make([]*db.Job, 0, len(group.Jobs))
	for _, job := range group.Jobs {
		if job == nil {
			clone.Jobs = append(clone.Jobs, nil)
			continue
		}
		jobClone := *job
		jobClone.Tags = append([]string(nil), job.Tags...)
		if job.CLIResourceOverrides != nil {
			overrides := *job.CLIResourceOverrides
			overrides.MachineAffinity = append([]string(nil), job.CLIResourceOverrides.MachineAffinity...)
			jobClone.CLIResourceOverrides = &overrides
		}
		clone.Jobs = append(clone.Jobs, &jobClone)
	}
	return clone
}

func ensurePlacementOverrides(job *db.Job) *db.CLIResourceOverrides {
	if job.CLIResourceOverrides == nil {
		job.CLIResourceOverrides = &db.CLIResourceOverrides{}
	}
	return job.CLIResourceOverrides
}

func removePlacementTag(jobs []*db.Job, tag string) {
	for _, job := range jobs {
		if job == nil {
			continue
		}
		filtered := job.Tags[:0]
		for _, existing := range job.Tags {
			if db.CanonicalizeTag(existing) != db.CanonicalizeTag(tag) {
				filtered = append(filtered, existing)
			}
		}
		job.Tags = filtered
	}
}
