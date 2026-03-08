package estimate

// PhaseID identifies a cloud job lifecycle phase.
type PhaseID string

const (
	PhaseStartup  PhaseID = "startup"
	PhaseSSHSetup PhaseID = "ssh_setup"
	PhaseJobSetup PhaseID = "job_setup"
	PhaseUpload   PhaseID = "upload"
)

// OverheadModel holds a hierarchical model per phase, built from historical
// cloud instance data.
type OverheadModel struct {
	Models map[PhaseID]*HierarchicalModel
}

// InstanceContext captures covariates needed for group key computation.
type InstanceContext struct {
	DataCenter   string
	DLPerf       float64
	InetDownMbps float64
	InetUpMbps   float64
	CacheWarm    bool // HF cache populated before job
}

// EstimatePhase returns the posterior predictive estimate for a phase+group.
// If the model is nil or has no data for this phase, returns fallback.
func (m *OverheadModel) EstimatePhase(phase PhaseID, group string, fallback Estimate) Estimate {
	if m == nil {
		return fallback
	}
	hm, ok := m.Models[phase]
	if !ok || hm == nil {
		return fallback
	}
	return hm.PredictiveEstimate(group)
}

// GroupKey returns the group key for a phase given instance metadata.
// Different phases condition on different covariates.
func GroupKey(phase PhaseID, ctx InstanceContext) string {
	switch phase {
	case PhaseStartup:
		if ctx.DataCenter != "" {
			return ctx.DataCenter
		}
		return "_global"
	case PhaseSSHSetup:
		return bandwidthBucket(ctx.InetDownMbps)
	case PhaseJobSetup:
		if ctx.CacheWarm {
			return "warm"
		}
		return "cold"
	case PhaseUpload:
		return bandwidthBucket(ctx.InetUpMbps)
	default:
		return "_global"
	}
}

// bandwidthBucket returns a coarse bucket label for a bandwidth value in Mbps.
func bandwidthBucket(mbps float64) string {
	switch {
	case mbps <= 0:
		return "_unknown"
	case mbps < 100:
		return "slow"
	case mbps < 500:
		return "medium"
	case mbps < 1000:
		return "fast"
	default:
		return "very_fast"
	}
}

// EstimateStartupWithModel returns the estimated startup time, using the
// overhead model if available, falling back to hardcoded defaults.
func EstimateStartupWithModel(provider string, model *OverheadModel, ctx InstanceContext) Estimate {
	fallback := EstimateStartup(provider)
	group := GroupKey(PhaseStartup, ctx)
	return model.EstimatePhase(PhaseStartup, group, fallback)
}

// EstimateSSHSetup returns the estimated SSH setup time (ready → wrapper_start).
func EstimateSSHSetup(model *OverheadModel, ctx InstanceContext) Estimate {
	fallback := FromSeconds(10, 5, 30)
	group := GroupKey(PhaseSSHSetup, ctx)
	return model.EstimatePhase(PhaseSSHSetup, group, fallback)
}

// EstimateJobSetup returns the estimated job setup time (setup_start → setup_end).
func EstimateJobSetup(model *OverheadModel, ctx InstanceContext) Estimate {
	fallback := FromSeconds(30, 10, 120)
	group := GroupKey(PhaseJobSetup, ctx)
	return model.EstimatePhase(PhaseJobSetup, group, fallback)
}

// EstimateUpload returns the estimated upload time (upload_start → upload_end).
func EstimateUpload(model *OverheadModel, ctx InstanceContext) Estimate {
	fallback := FromSeconds(15, 5, 60)
	group := GroupKey(PhaseUpload, ctx)
	return model.EstimatePhase(PhaseUpload, group, fallback)
}
