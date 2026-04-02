package campaign

import (
	"fmt"
	"sort"
	"strings"
)

// RuntimePredictionSummary describes the estimator provenance across a set of jobs.
type RuntimePredictionSummary struct {
	TotalJobs                 int
	MetadataJobs              int
	MedianConfidence          float64
	DominantSource            string
	DominantBottleneck        string
	InfeasibleJobs            int
	AdditionalVRAMBenefitJobs int
	AdditionalVRAMNeutralJobs int
}

// SummarizeRuntimePredictions aggregates estimator metadata across the selected groups.
func SummarizeRuntimePredictions(estimates []CostEstimate, selectedPerGroup []int) RuntimePredictionSummary {
	var summary RuntimePredictionSummary
	var confidences []float64
	sourceCounts := make(map[string]int)
	bottleneckCounts := make(map[string]int)

	for groupIdx, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}
		selected, _ := selectionScale(groupIdx, len(est.Group.Jobs), selectedPerGroup)
		if selected == 0 {
			continue
		}

		summary.TotalJobs += len(est.Group.Jobs)
		for _, job := range est.Group.Jobs {
			if job == nil {
				continue
			}
			meta, ok := est.JobRuntimeMetadata[job.ID]
			if !ok {
				continue
			}
			summary.MetadataJobs++
			confidences = append(confidences, meta.Confidence)
			if meta.Source != "" {
				sourceCounts[meta.Source]++
			}
			if meta.Bottleneck != "" {
				bottleneckCounts[meta.Bottleneck]++
			}
			if meta.Feasible != nil && !*meta.Feasible {
				summary.InfeasibleJobs++
			}
			if meta.BenefitsFromAdditionalVRAM != nil {
				if *meta.BenefitsFromAdditionalVRAM {
					summary.AdditionalVRAMBenefitJobs++
				} else {
					summary.AdditionalVRAMNeutralJobs++
				}
			}
		}
	}

	summary.MedianConfidence = medianFloat(confidences)
	summary.DominantSource = dominantLabel(sourceCounts)
	summary.DominantBottleneck = dominantLabel(bottleneckCounts)
	return summary
}

// Hint formats a concise, user-visible explanation of runtime estimate provenance.
func (s RuntimePredictionSummary) Hint() string {
	if s.TotalJobs == 0 {
		return ""
	}
	if s.MetadataJobs == 0 {
		return "runtime: neutral fallback (no estimator metadata)"
	}

	parts := []string{fmt.Sprintf("runtime: %s", summarizeSourceLabel(s.DominantSource))}
	parts = append(parts, fmt.Sprintf("%.0f%% confidence", s.MedianConfidence*100))

	if s.DominantBottleneck != "" && s.DominantBottleneck != "unknown" {
		parts = append(parts, fmt.Sprintf("%s-bound", strings.ReplaceAll(s.DominantBottleneck, "_", " ")))
	}
	if s.InfeasibleJobs > 0 {
		parts = append(parts, fmt.Sprintf("%d infeasible", s.InfeasibleJobs))
	}
	if s.AdditionalVRAMNeutralJobs > 0 && s.AdditionalVRAMBenefitJobs == 0 {
		parts = append(parts, fmt.Sprintf("surplus VRAM neutral for %d", s.AdditionalVRAMNeutralJobs))
	} else if s.AdditionalVRAMBenefitJobs > 0 {
		parts = append(parts, fmt.Sprintf("more VRAM helps %d", s.AdditionalVRAMBenefitJobs))
	}
	return strings.Join(parts, ", ")
}

func summarizeSourceLabel(source string) string {
	switch source {
	case "", "mixed":
		return "mixed sources"
	case "learned+analytical":
		return "learned + analytical"
	case "learned":
		return "learned only"
	case "empirical":
		return "empirical"
	default:
		return source
	}
}

func dominantLabel(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	if len(counts) == 1 {
		for key := range counts {
			return key
		}
	}

	type item struct {
		label string
		count int
	}
	items := make([]item, 0, len(counts))
	for label, count := range counts {
		items = append(items, item{label: label, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count == items[j].count {
			return items[i].label < items[j].label
		}
		return items[i].count > items[j].count
	})
	if items[0].count == items[1].count {
		return "mixed"
	}
	return items[0].label
}

func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
