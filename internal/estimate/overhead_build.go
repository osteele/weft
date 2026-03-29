package estimate

import (
	"math"

	"github.com/osteele/weft/internal/db"
)

// outlierCaps defines maximum plausible durations (seconds) per phase.
// Observations exceeding these are discarded.
var outlierCaps = map[PhaseID]float64{
	PhaseStartup:  15 * 60,
	PhaseSSHSetup: 10 * 60,
	PhaseJobSetup: 60 * 60,
	PhaseUpload:   30 * 60,
}

// BuildOverheadModel constructs an OverheadModel from historical observations.
// Returns nil if there are no observations.
func BuildOverheadModel(obs []db.OverheadObservation) *OverheadModel {
	if len(obs) == 0 {
		return nil
	}

	type phaseExtractor struct {
		phase  PhaseID
		getDur func(o *db.OverheadObservation) *float64
		getCtx func(o *db.OverheadObservation) InstanceContext
	}

	extractors := []phaseExtractor{
		{
			phase:  PhaseStartup,
			getDur: func(o *db.OverheadObservation) *float64 { return o.StartupSec },
			getCtx: func(o *db.OverheadObservation) InstanceContext {
				return InstanceContext{DataCenter: o.DataCenter}
			},
		},
		{
			phase:  PhaseSSHSetup,
			getDur: func(o *db.OverheadObservation) *float64 { return o.SSHSetupSec },
			getCtx: func(o *db.OverheadObservation) InstanceContext {
				return InstanceContext{InetDownMbps: o.InetDownMbps}
			},
		},
		{
			phase:  PhaseJobSetup,
			getDur: func(o *db.OverheadObservation) *float64 { return o.JobSetupSec },
			getCtx: func(o *db.OverheadObservation) InstanceContext {
				warm := o.CacheHFBytes != nil && *o.CacheHFBytes > 0
				var downloaded int64
				if o.CacheHFPostBytes != nil && o.CacheHFBytes != nil {
					downloaded = *o.CacheHFPostBytes - *o.CacheHFBytes
				}
				// When CacheHFBytes is nil we don't know the pre-cache size,
				// so downloaded stays 0 (unknown) → groups into "cold:none".
				return InstanceContext{CacheWarm: warm, DownloadedBytes: downloaded}
			},
		},
		{
			phase:  PhaseUpload,
			getDur: func(o *db.OverheadObservation) *float64 { return o.UploadSec },
			getCtx: func(o *db.OverheadObservation) InstanceContext {
				return InstanceContext{InetUpMbps: o.InetUpMbps}
			},
		},
	}

	models := make(map[PhaseID]*HierarchicalModel, len(extractors))

	for _, ext := range extractors {
		groups := make(map[string]*SuffStats)
		cap := outlierCaps[ext.phase]

		for i := range obs {
			dur := ext.getDur(&obs[i])
			if dur == nil || *dur <= 0 {
				continue
			}
			if cap > 0 && *dur > cap {
				continue
			}

			ctx := ext.getCtx(&obs[i])
			key := GroupKey(ext.phase, ctx)

			gs, ok := groups[key]
			if !ok {
				gs = &SuffStats{}
				groups[key] = gs
			}
			gs.Add(math.Log(*dur))
		}

		if model := Fit(groups); model != nil {
			models[ext.phase] = model
		}
	}

	if len(models) == 0 {
		return nil
	}

	return &OverheadModel{Models: models}
}
