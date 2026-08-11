package cmd

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

type parsedRentalPolicyFlags struct {
	maxHourlyRateCents *int
	maxSpendCents      *int
	maxTimeSeconds     *int
	gracePeriodSeconds *int
	minSurvival        *float64
}

func parseRentalPolicyFlags(cmd *cobra.Command, maxHourlyRate, maxSpend, maxTime, gracePeriod string, minSurvival float64) (parsedRentalPolicyFlags, error) {
	var parsed parsedRentalPolicyFlags
	var err error
	if cmd.Flags().Changed("max-hourly-rate") {
		parsed.maxHourlyRateCents, err = parseDollarCap("max-hourly-rate", maxHourlyRate)
		if err != nil {
			return parsed, err
		}
	}
	if cmd.Flags().Changed("max-spend") {
		parsed.maxSpendCents, err = parseDollarCap("max-spend", maxSpend)
		if err != nil {
			return parsed, err
		}
	}
	if cmd.Flags().Changed("max-time") {
		parsed.maxTimeSeconds, err = parseDurationCap("max-time", maxTime, true)
		if err != nil {
			return parsed, err
		}
	}
	if cmd.Flags().Changed("grace-period") {
		parsed.gracePeriodSeconds, err = parseDurationCap("grace-period", gracePeriod, false)
		if err != nil {
			return parsed, err
		}
	}
	if cmd.Flags().Changed("min-survival") {
		if minSurvival < 0 || minSurvival > 1 {
			return parsed, fmt.Errorf("--min-survival must be between 0 and 1")
		}
		value := minSurvival
		parsed.minSurvival = &value
	}
	return parsed, nil
}

func parseDollarCap(name, raw string) (*int, error) {
	value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "$"))
	if value == "" {
		return nil, fmt.Errorf("--%s requires a dollar amount", name)
	}
	dollars, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(dollars) || math.IsInf(dollars, 0) || dollars < 0 {
		return nil, fmt.Errorf("--%s must be a non-negative dollar amount", name)
	}
	if dollars == 0 {
		return nil, nil
	}
	centsFloat := dollars * 100
	cents := math.Round(centsFloat)
	if math.Abs(centsFloat-cents) > 1e-6 {
		return nil, fmt.Errorf("--%s supports at most two decimal places", name)
	}
	if cents > float64(math.MaxInt) {
		return nil, fmt.Errorf("--%s is too large", name)
	}
	result := int(cents)
	return &result, nil
}

func parseDurationCap(name, raw string, zeroClears bool) (*int, error) {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "default" || value == "clear" || value == "auto" {
		return nil, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return nil, fmt.Errorf("--%s must be a non-negative duration", name)
	}
	if duration == 0 && zeroClears {
		return nil, nil
	}
	if duration > 0 && duration < time.Second {
		return nil, fmt.Errorf("--%s must be at least 1s", name)
	}
	seconds := int(duration / time.Second)
	return &seconds, nil
}

func inheritRentalPolicyOverrides(target, source *db.CLIResourceOverrides) {
	if target == nil || source == nil {
		return
	}
	target.MaxHourlyRateCents = cloneIntPtr(source.MaxHourlyRateCents)
	target.MaxSpendCents = cloneIntPtr(source.MaxSpendCents)
	target.MaxTimeSeconds = cloneIntPtr(source.MaxTimeSeconds)
	target.GracePeriodSeconds = cloneIntPtr(source.GracePeriodSeconds)
	if source.MinSurvival != nil {
		value := *source.MinSurvival
		target.MinSurvival = &value
	}
}

func applyRentalPolicyFlags(cmd *cobra.Command, target *db.CLIResourceOverrides, parsed parsedRentalPolicyFlags) {
	if cmd.Flags().Changed("max-hourly-rate") {
		target.MaxHourlyRateCents = cloneIntPtr(parsed.maxHourlyRateCents)
	}
	if cmd.Flags().Changed("max-spend") {
		target.MaxSpendCents = cloneIntPtr(parsed.maxSpendCents)
	}
	if cmd.Flags().Changed("max-time") {
		target.MaxTimeSeconds = cloneIntPtr(parsed.maxTimeSeconds)
	}
	if cmd.Flags().Changed("grace-period") {
		target.GracePeriodSeconds = cloneIntPtr(parsed.gracePeriodSeconds)
	}
	if cmd.Flags().Changed("min-survival") {
		if parsed.minSurvival == nil {
			target.MinSurvival = nil
		} else {
			value := *parsed.minSurvival
			target.MinSurvival = &value
		}
	}
}

func rentalPolicyFlagsChanged(cmd *cobra.Command) bool {
	for _, name := range []string{"max-hourly-rate", "max-spend", "max-time", "grace-period", "min-survival"} {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func rentalPolicyUpdateMessages(cmd *cobra.Command, parsed parsedRentalPolicyFlags) []string {
	var messages []string
	if cmd.Flags().Changed("max-hourly-rate") {
		messages = append(messages, formatMoneyCapUpdate("max-hourly-rate", parsed.maxHourlyRateCents))
	}
	if cmd.Flags().Changed("max-spend") {
		messages = append(messages, formatMoneyCapUpdate("max-spend", parsed.maxSpendCents))
	}
	if cmd.Flags().Changed("max-time") {
		messages = append(messages, formatDurationCapUpdate("max-time", parsed.maxTimeSeconds, "cleared"))
	}
	if cmd.Flags().Changed("grace-period") {
		messages = append(messages, formatDurationCapUpdate("grace-period", parsed.gracePeriodSeconds, "default"))
	}
	if cmd.Flags().Changed("min-survival") && parsed.minSurvival != nil {
		if *parsed.minSurvival == 0 {
			messages = append(messages, "min-survival: disabled")
		} else {
			messages = append(messages, fmt.Sprintf("min-survival: %.0f%%", *parsed.minSurvival*100))
		}
	}
	return messages
}

func formatMoneyCapUpdate(name string, cents *int) string {
	if cents == nil {
		return name + ": cleared"
	}
	return fmt.Sprintf("%s: $%.2f", name, float64(*cents)/100)
}

func formatDurationCapUpdate(name string, seconds *int, empty string) string {
	if seconds == nil {
		return name + ": " + empty
	}
	if *seconds == 0 {
		return name + ": disabled"
	}
	return fmt.Sprintf("%s: %s", name, time.Duration(*seconds)*time.Second)
}

func formatJobRentalPolicy(job *db.Job) string {
	if job == nil || job.CLIResourceOverrides == nil {
		return ""
	}
	o := job.CLIResourceOverrides
	var parts []string
	if o.MaxHourlyRateCents != nil {
		parts = append(parts, fmt.Sprintf("rate <= $%.2f/hr", float64(*o.MaxHourlyRateCents)/100))
	}
	if o.MaxSpendCents != nil {
		parts = append(parts, fmt.Sprintf("spend <= $%.2f", float64(*o.MaxSpendCents)/100))
	}
	if o.MaxTimeSeconds != nil {
		parts = append(parts, "time <= "+(time.Duration(*o.MaxTimeSeconds)*time.Second).String())
	}
	if o.MinSurvival != nil {
		parts = append(parts, fmt.Sprintf("survival >= %.0f%%", *o.MinSurvival*100))
	}
	if o.GracePeriodSeconds != nil {
		if *o.GracePeriodSeconds == 0 {
			parts = append(parts, "grace disabled")
		} else {
			parts = append(parts, "grace "+(time.Duration(*o.GracePeriodSeconds)*time.Second).String())
		}
	}
	return strings.Join(parts, ", ")
}
