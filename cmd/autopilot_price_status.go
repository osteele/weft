package cmd

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

var offeredPriceRE = regexp.MustCompile(`offered \$([0-9]+(?:\.[0-9]+)?)/hr`)

type priceAuthBlockView struct {
	JobIDNumber      int64  `json:"-"`
	JobID            string `json:"job_id"`
	Project          string `json:"project,omitempty"`
	Description      string `json:"description,omitempty"`
	GPUClass         string `json:"gpu_class,omitempty"`
	GPUMemGB         int    `json:"gpu_mem_gb,omitempty"`
	GPUBucket        string `json:"gpu_bucket,omitempty"`
	Reason           string `json:"reason"`
	OfferedCents     int    `json:"offered_cents,omitempty"`
	AuthorizeCommand string `json:"authorize_command,omitempty"`
}

func collectPriceAuthorizationBlocks(database *sql.DB) ([]priceAuthBlockView, error) {
	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, fmt.Errorf("list unplaced jobs: %w", err)
	}
	blocks := make([]priceAuthBlockView, 0)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		reason := priceAuthorizationReason(job)
		if reason == "" {
			continue
		}
		gpuMemGB := 0
		if job.GPUMemGB != nil {
			gpuMemGB = *job.GPUMemGB
		}
		jobID := ids.FormatJobID(job.ID)
		offeredCents := offeredCentsFromReason(reason)
		blocks = append(blocks, priceAuthBlockView{
			JobIDNumber:      job.ID,
			JobID:            jobID,
			Project:          job.Project,
			Description:      job.Description,
			GPUClass:         job.GPUClass,
			GPUMemGB:         gpuMemGB,
			GPUBucket:        formatPriceAuthGPUBucket(job.GPUClass, gpuMemGB),
			Reason:           reason,
			OfferedCents:     offeredCents,
			AuthorizeCommand: formatPriceAuthCommand(jobID, offeredCents),
		})
	}
	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].JobIDNumber < blocks[j].JobIDNumber
	})
	return blocks, nil
}

func priceAuthorizationReason(job *db.Job) string {
	s := blockreason.Parse(job.PlacementBlockedJSON)
	if s == nil {
		return ""
	}
	for _, candidate := range []string{s.LaunchDetail, s.Launch, s.Summary} {
		reason := strings.TrimSpace(candidate)
		if isPriceAuthorizationReason(reason) {
			return reason
		}
	}
	return ""
}

func isPriceAuthorizationReason(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "requires price authorization") ||
		strings.Contains(lower, "requires_price_authorization")
}

func offeredCentsFromReason(reason string) int {
	matches := offeredPriceRE.FindStringSubmatch(reason)
	if len(matches) != 2 {
		return 0
	}
	value, err := strconv.ParseFloat(matches[1], 64)
	if err != nil || value <= 0 {
		return 0
	}
	return int(value*100 + 0.5)
}

func formatPriceAuthGPUBucket(gpuClass string, gpuMemGB int) string {
	gpuClass = strings.TrimSpace(gpuClass)
	switch {
	case gpuClass != "" && gpuMemGB > 0:
		return fmt.Sprintf("%s >=%dGB", gpuClass, gpuMemGB)
	case gpuClass != "":
		return gpuClass
	case gpuMemGB > 0:
		return fmt.Sprintf(">=%dGB", gpuMemGB)
	default:
		return "gpu"
	}
}

func formatPriceAuthCommand(jobID string, offeredCents int) string {
	if offeredCents <= 0 {
		return fmt.Sprintf("weft job authorize-price %s --up-to <dollars-per-hour>", jobID)
	}
	return fmt.Sprintf("weft job authorize-price %s --up-to %.2f", jobID, float64(offeredCents)/100)
}

func pluralWord(n int, singular, pluralForm string) string {
	if n == 1 {
		return singular
	}
	return pluralForm
}
