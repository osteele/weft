package campaign

import (
	"fmt"
	"math"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

type rentalPolicyKey struct {
	hasMaxHourlyRate bool
	maxHourlyRate    int
	hasMaxSpend      bool
	maxSpend         int
	hasMaxTime       bool
	maxTime          int
	hasGracePeriod   bool
	gracePeriod      int
	hasMinSurvival   bool
	minSurvival      float64
}

func rentalPolicyForJob(job *db.Job) rentalPolicyKey {
	var key rentalPolicyKey
	if job == nil || job.CLIResourceOverrides == nil {
		return key
	}
	o := job.CLIResourceOverrides
	if o.MaxHourlyRateCents != nil {
		key.hasMaxHourlyRate = true
		key.maxHourlyRate = *o.MaxHourlyRateCents
	}
	if o.MaxSpendCents != nil {
		key.hasMaxSpend = true
		key.maxSpend = *o.MaxSpendCents
	}
	if o.MaxTimeSeconds != nil {
		key.hasMaxTime = true
		key.maxTime = *o.MaxTimeSeconds
	}
	if o.GracePeriodSeconds != nil {
		key.hasGracePeriod = true
		key.gracePeriod = *o.GracePeriodSeconds
	}
	if o.MinSurvival != nil {
		key.hasMinSurvival = true
		key.minSurvival = *o.MinSurvival
	}
	return key
}

func groupRentalPolicy(group InstanceGroup) rentalPolicyKey {
	if len(group.Jobs) == 0 {
		return rentalPolicyKey{}
	}
	return rentalPolicyForJob(group.Jobs[0])
}

func rentalPoliciesMatch(a, b InstanceGroup) bool {
	return groupRentalPolicy(a) == groupRentalPolicy(b)
}

func applyJobRentalPolicy(opts LaunchOpts, jobs []*db.Job) (LaunchOpts, error) {
	var gracePeriod *int
	for _, job := range jobs {
		if job == nil || job.CLIResourceOverrides == nil {
			continue
		}
		o := job.CLIResourceOverrides
		opts.MaxSpendCents = stricterPositiveLimit(opts.MaxSpendCents, o.MaxSpendCents)
		opts.MaxTimeSeconds = stricterPositiveLimit(opts.MaxTimeSeconds, o.MaxTimeSeconds)
		if o.MinSurvival != nil {
			opts.MinSurvival = *o.MinSurvival
		}
		if o.GracePeriodSeconds != nil {
			if gracePeriod != nil && *gracePeriod != *o.GracePeriodSeconds {
				return opts, fmt.Errorf("jobs with different grace-period policies cannot share a rental")
			}
			value := *o.GracePeriodSeconds
			gracePeriod = &value
		}
	}
	if gracePeriod != nil {
		opts.GracePeriodSeconds = *gracePeriod
	}
	return opts, nil
}

func stricterPositiveLimit(current int, override *int) int {
	if override == nil || *override <= 0 {
		return current
	}
	if current <= 0 || *override < current {
		return *override
	}
	return current
}

func validateOfferHourlyRateCap(group InstanceGroup, offer cloud.Offer) error {
	capCents, _ := db.RequestedMaxHourlyRateCentsForJobs(group.Jobs)
	if capCents <= 0 {
		return nil
	}
	offerCents := offer.CostPerHour * 100
	if offerCents <= float64(capCents)+1e-9 {
		return nil
	}
	return fmt.Errorf(
		"offer rate $%.4g/hr exceeds job max-hourly-rate $%.2f/hr",
		offer.CostPerHour,
		float64(capCents)/100,
	)
}

func launchGroupHasAuthorizedRateCap(group InstanceGroup, offer cloud.Offer) bool {
	capCents, allExplicit := db.RequestedMaxHourlyRateCentsForJobs(group.Jobs)
	return allExplicit && capCents > 0 && int(math.Ceil(offer.CostPerHour*100-1e-9)) <= capCents
}
