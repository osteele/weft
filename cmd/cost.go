package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

var costCmd = &cobra.Command{
	Use:   "cost <instances|jobs|campaigns>",
	Short: "Show cost summaries for instances, jobs, or campaigns",
}

var costInstancesCmd = &cobra.Command{
	Use:     "instances",
	Aliases: []string{"instance"},
	Short:   "Show cost per cloud instance",
	RunE:    runCostInstances,
}

var costJobsCmd = &cobra.Command{
	Use:     "jobs",
	Aliases: []string{"job"},
	Short:   "Show cost per cloud job",
	RunE:    runCostJobs,
}

var costCampaignsCmd = &cobra.Command{
	Use:     "campaigns",
	Aliases: []string{"campaign"},
	Short:   "Show cost per campaign",
	RunE:    runCostCampaigns,
}

// defaultCostJobsLimit bounds how many jobs "weft cost jobs" examines by
// default. The selection is always reported, and --all lifts it.
const defaultCostJobsLimit = 200

var (
	costJobsAll   bool
	costJobsLimit int
)

func init() {
	rootCmd.AddCommand(costCmd)
	costCmd.AddCommand(costInstancesCmd)
	costCmd.AddCommand(costJobsCmd)
	costCmd.AddCommand(costCampaignsCmd)

	costJobsCmd.Flags().BoolVarP(&costJobsAll, "all", "a", false, "Examine every job, not just the most recent ones")
	costJobsCmd.Flags().IntVar(&costJobsLimit, "limit", defaultCostJobsLimit, "How many jobs to examine, active first then newest (ignored with --all)")

	// Noun-verb aliases: "weft instance cost", "weft job cost", "weft campaign cost"
	instanceCmd.AddCommand(verbAlias("cost", costInstancesCmd))
	jobCostAlias := verbAlias("cost", costJobsCmd)
	jobCostAlias.Flags().AddFlagSet(costJobsCmd.Flags())
	jobCmd.AddCommand(jobCostAlias)
	campaignCmd.AddCommand(verbAlias("cost", costCampaignsCmd))
}

func runCostInstances(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	reconcileBeforeDisplay(database, FastCloudSyncTimeout)

	instances, err := db.ListLaunches(database)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	var priced []*db.Launch
	for _, inst := range instances {
		if inst.CostPerHourCents > 0 {
			priced = append(priced, inst)
		}
	}
	if len(priced) == 0 {
		fmt.Println("No instances with pricing data.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Instance\tGPU\tStatus\tRate\tDuration\tCost\n")
	for _, inst := range priced {
		duration, actualCost := instanceCostBreakdown(inst)
		costPerHr := fmt.Sprintf("$%.2f/hr", float64(inst.CostPerHourCents)/100)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			ids.FormatInstanceID(inst.ID), inst.DisplayGPUBrief(), inst.Status,
			costPerHr, duration, actualCost)
	}
	w.Flush()
	return nil
}

func runCostJobs(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	return reportJobCosts(os.Stdout, database, costJobsAll, costJobsLimit, time.Now())
}

// costState separates a cost weft could compute from one it could not, and
// from a job that never ran on a billed rental. An omission must never read
// as a zero.
type costState int

const (
	costNone costState = iota // job never ran on a rental instance
	costKnown
	costUnknown // job ran on rentals, but no cost is attributable to it
)

type jobCostAssessment struct {
	state  costState
	amount float64 // dollars
	basis  string
}

// costJobsSelection records which jobs the report actually looked at, so an
// empty or small result can never be mistaken for "no spend".
type costJobsSelection struct {
	all      bool
	limit    int
	examined int
	total    int
}

// describe names the selection in the terms the underlying query applies.
//
// It is not "newest N": db.ListJobsByStatuses orders active jobs first and
// only then by descending id, so with a long-running job on the list the
// examined set is not the newest by id — which is exactly the case a cost
// report cares about. Saying "newest" would misdescribe the one situation the
// reader most needs to reason about.
func (s costJobsSelection) describe() string {
	if s.all {
		return fmt.Sprintf("all %d known job(s)", s.total)
	}
	return fmt.Sprintf("%d of %d known job(s), active first then newest (--limit %d)", s.examined, s.total, s.limit)
}

func (s costJobsSelection) unexamined() int {
	if s.all || s.total <= s.examined {
		return 0
	}
	return s.total - s.examined
}

func reportJobCosts(out io.Writer, database *sql.DB, all bool, limit int, now time.Time) error {
	if limit <= 0 {
		all = true
	}
	queryLimit := 0
	if !all {
		queryLimit = limit
	}
	jobs, err := db.ListJobsByStatuses(database, nil, "", "", queryLimit, nil, "")
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	total, err := countKnownJobs(database)
	if err != nil {
		return fmt.Errorf("count jobs: %w", err)
	}
	if total < len(jobs) {
		total = len(jobs)
	}
	selection := costJobsSelection{all: all, limit: limit, examined: len(jobs), total: total}

	// Two bounded queries for the whole attempt history instead of per-job view
	// lookups: they say which examined jobs ever touched a rental instance, and
	// which instances were theirs alone. A job absent from this record never ran
	// on a rental, which is a confirmed negative — a failed read errors out
	// below rather than passing for "nothing was billed".
	membership, err := loadRentalMembership(database)
	if err != nil {
		return fmt.Errorf("read rental attempt history: %w", err)
	}
	launchRows, err := db.GetLaunchesByIDs(database, membership.launchIDsFor(jobs))
	if err != nil {
		return fmt.Errorf("get launches: %w", err)
	}

	type costRow struct {
		job        *db.Job
		assessment jobCostAssessment
	}
	var rows []costRow
	var totalCents float64
	pricedJobs, unknownJobs, zeroJobs := 0, 0, 0
	for _, j := range jobs {
		launchesUsed := membership.launchesFor(j)
		if len(launchesUsed) == 0 && (j.Cost == nil || *j.Cost <= 0) {
			continue
		}
		assessment := assessJobCost(database, j, launchesUsed, launchRows, membership, now)
		switch {
		case assessment.state == costKnown && assessment.amount > 0:
			pricedJobs++
			totalCents += assessment.amount * 100
			rows = append(rows, costRow{job: j, assessment: assessment})
		case assessment.state == costKnown:
			zeroJobs++
		case assessment.state == costUnknown:
			unknownJobs++
			rows = append(rows, costRow{job: j, assessment: assessment})
		}
	}

	field(out, "Selection:", selection.describe())
	field(out, "Cost basis:", "rental uptime × rate over each job's attempts, as 'weft info <id>' computes it")

	launches := launchRows
	jobCounts, err := db.GetLaunchJobCounts(database)
	if err != nil {
		return fmt.Errorf("get job counts: %w", err)
	}

	for _, row := range rows {
		fmt.Fprintln(out, "---")
		j := row.job
		project := j.Project
		if project == "" {
			project = "—"
		}

		field(out, "Job:", ids.FormatJobID(j.ID))
		field(out, "Project:", project)
		field(out, "Status:", j.Status)
		if row.assessment.state == costKnown {
			field(out, "Job cost:", fmt.Sprintf("%s (%s)", campaign.FormatCostCents(row.assessment.amount*100), row.assessment.basis))
		} else {
			field(out, "Job cost:", fmt.Sprintf("unknown — %s", row.assessment.basis))
		}

		if j.LaunchID != nil && *j.LaunchID > 0 {
			inst := launches[*j.LaunchID]
			field(out, "Instance:", ids.FormatInstanceID(*j.LaunchID))
			if inst != nil {
				_, instCostStr := instanceCostBreakdown(inst)
				field(out, "Instance cost:", instCostStr)

				n := jobCounts[*j.LaunchID]
				if n > 0 {
					field(out, "Jobs on instance:", fmt.Sprintf("%d", n))
					if row.assessment.state == costKnown && inst.LaunchedAt != nil && inst.CostPerHourCents > 0 {
						var end time.Time
						if inst.EndedAt != nil {
							end = time.Unix(*inst.EndedAt, 0)
						} else {
							end = now
						}
						instCents := end.Sub(time.Unix(*inst.LaunchedAt, 0)).Hours() * float64(inst.CostPerHourCents)
						overheadCents := (instCents - (row.assessment.amount * 100)) / float64(n)
						if overheadCents > 0 {
							field(out, "Overhead/job:", campaign.FormatCostCents(overheadCents))
						}
					}
				}
			}
		}
	}

	if len(rows) > 0 {
		fmt.Fprintln(out, "---")
	}
	field(out, "Summary:", costJobsSummary(selection, totalCents, pricedJobs, unknownJobs, zeroJobs))
	if n := selection.unexamined(); n > 0 {
		field(out, "Not examined:", fmt.Sprintf("%d job(s) outside this selection; they may carry cost. Rerun with --all.", n))
	}
	return nil
}

func costJobsSummary(selection costJobsSelection, totalCents float64, priced, unknown, zero int) string {
	if priced == 0 {
		summary := fmt.Sprintf("No attributable cost among the %d examined job(s)", selection.examined)
		if unknown > 0 {
			summary += fmt.Sprintf("; %d ran on rental instances whose cost is not attributable", unknown)
		}
		return summary + "."
	}
	summary := fmt.Sprintf("%s across %d of %d examined job(s)", campaign.FormatCostCents(totalCents), priced, selection.examined)
	if unknown > 0 {
		summary += fmt.Sprintf("; %d with rental history but no attributable cost", unknown)
	}
	if zero > 0 {
		summary += fmt.Sprintf("; %d billed nothing", zero)
	}
	return summary + "."
}

// assessJobCost answers what this job was billed, on the same basis as
// 'weft info': rental uptime × rate over the instances its attempts ran on.
// The stored per-attempt scalar is only a fallback, because nothing writes it
// for rental jobs. An instance this job shared with another job is not
// attributable to it alone, and is reported as such instead of as zero.
func assessJobCost(database *sql.DB, job *db.Job, launches []int64, launchRows map[int64]*db.Launch, membership *rentalMembership, now time.Time) jobCostAssessment {
	var attributed float64
	var exclusive, shared, unrecorded int
	var sharedLaunch *db.Launch
	provisional := false
	for _, launchID := range launches {
		launch := launchRows[launchID]
		if launch == nil {
			unrecorded++
			continue
		}
		if !membership.exclusiveTo(launchID, job.ID) {
			shared++
			sharedLaunch = launch
			continue
		}
		exclusive++
		attributed += estimate.LaunchCostSoFar(launch, now)
		provisional = provisional || !launch.IsTerminal()
	}

	switch {
	case exclusive > 0 && shared == 0 && unrecorded == 0:
		basis := fmt.Sprintf("%d rental instance(s)", exclusive)
		if provisional {
			basis = fmt.Sprintf("provisional; %d rental instance(s) to date", exclusive)
		}
		return jobCostAssessment{state: costKnown, amount: attributed, basis: basis}

	case exclusive == 0 && shared == 1 && unrecorded == 0:
		// Sole shared instance: bill this job's own setup + run at the
		// instance rate, exactly as 'weft info' does.
		rate := float64(sharedLaunch.CostPerHourCents) / 100
		timings, _ := db.GetJobPhaseTimings(database, job.ID)
		billable := estimate.JobSetupDuration(job, timings, now) + estimate.JobRunElapsedDuration(job, timings, now)
		if rate > 0 && billable > 0 {
			basis := "shared instance: setup + run"
			if !sharedLaunch.IsTerminal() {
				basis += "; provisional until teardown"
			}
			return jobCostAssessment{state: costKnown, amount: billable.Hours() * rate, basis: basis}
		}

	case exclusive > 0:
		basis := fmt.Sprintf("%d of %d rental instance(s); the rest shared or unrecorded and not attributable — see 'weft info %s'",
			exclusive, len(launches), ids.FormatJobID(job.ID))
		if provisional {
			basis += "; provisional"
		}
		return jobCostAssessment{state: costKnown, amount: attributed, basis: basis}
	}

	// Legacy rows only. Nothing in weft writes job_attempts.cost any more: its
	// writers had no callers and were removed with this repair, which is why a
	// filter on this scalar reported every rental job as costless (wb179).
	if job.Cost != nil && *job.Cost > 0 {
		return jobCostAssessment{state: costKnown, amount: *job.Cost, basis: "recorded on the job's latest attempt (legacy row)"}
	}
	if len(launches) > 0 {
		return jobCostAssessment{
			state: costUnknown,
			basis: fmt.Sprintf("%d rental instance(s) in this job's history, none attributable to it alone; see 'weft info %s'",
				len(launches), ids.FormatJobID(job.ID)),
		}
	}
	return jobCostAssessment{state: costNone}
}

// rentalMembership is the job↔instance attempt record, read once in a single
// query. Per-job lookups against the launch-membership view cost tens of
// milliseconds each, which is minutes over a full job history.
type rentalMembership struct {
	jobLaunches map[int64][]int64            // job → the instances its live attempts used
	launchJobs  map[int64]map[int64]struct{} // instance → every job that ever attempted on it
}

func loadRentalMembership(database *sql.DB) (*rentalMembership, error) {
	rows, err := database.Query(`
		SELECT job_id, launch_id, abandoned_at IS NULL
		  FROM job_attempts
		 WHERE launch_id IS NOT NULL AND launch_id > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := &rentalMembership{
		jobLaunches: make(map[int64][]int64),
		launchJobs:  make(map[int64]map[int64]struct{}),
	}
	for rows.Next() {
		var jobID, launchID int64
		var live bool
		if err := rows.Scan(&jobID, &launchID, &live); err != nil {
			return nil, err
		}
		// Every attempt counts toward occupancy, abandoned or not: another
		// job's abandoned attempt still means this instance was not ours alone.
		occupants := m.launchJobs[launchID]
		if occupants == nil {
			occupants = make(map[int64]struct{})
			m.launchJobs[launchID] = occupants
		}
		occupants[jobID] = struct{}{}
		if live && !slices.Contains(m.jobLaunches[jobID], launchID) {
			m.jobLaunches[jobID] = append(m.jobLaunches[jobID], launchID)
		}
	}
	return m, rows.Err()
}

// launchesFor returns the rental instances this job's attempts ran on.
func (m *rentalMembership) launchesFor(job *db.Job) []int64 {
	launches := m.jobLaunches[job.ID]
	if job.LaunchID != nil && *job.LaunchID > 0 && !slices.Contains(launches, *job.LaunchID) {
		launches = append(append(make([]int64, 0, len(launches)+1), launches...), *job.LaunchID)
	}
	return launches
}

func (m *rentalMembership) launchIDsFor(jobs []*db.Job) []int64 {
	seen := make(map[int64]struct{})
	var ids []int64
	for _, job := range jobs {
		for _, launchID := range m.launchesFor(job) {
			if _, ok := seen[launchID]; ok {
				continue
			}
			seen[launchID] = struct{}{}
			ids = append(ids, launchID)
		}
	}
	return ids
}

// exclusiveTo reports whether this job is the only job that ever ran on the
// instance. An instance with no occupancy record is not proven exclusive.
func (m *rentalMembership) exclusiveTo(launchID, jobID int64) bool {
	occupants := m.launchJobs[launchID]
	if len(occupants) == 0 {
		return false
	}
	for id := range occupants {
		if id != jobID {
			return false
		}
	}
	return true
}

func countKnownJobs(database *sql.DB) (int, error) {
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM job_status WHERE tombstoned = 0`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func runCostCampaigns(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	reconcileBeforeDisplay(database, FastCloudSyncTimeout)

	campaigns, err := db.ListCampaigns(database)
	if err != nil {
		return fmt.Errorf("list campaigns: %w", err)
	}
	if len(campaigns) == 0 {
		fmt.Println("No campaigns.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Campaign\tStatus\tInstances\tEst. cost\tActual cost\n")
	for _, c := range campaigns {
		instances, _ := db.GetCampaignInstances(database, c.ID)
		estCost := campaign.FormatEstimatedCostCents(c.EstimatedCostCents)
		actualCost := campaignActualCost(instances)
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\n",
			c.ID, c.Status, len(instances), estCost, actualCost)
	}
	w.Flush()
	return nil
}

// field prints a single "Label:   value" line with values aligned at column 19.
func field(w io.Writer, label, value string) {
	fmt.Fprintf(w, "%-19s%s\n", label, value)
}

// instanceCostBreakdown returns the formatted duration and actual cost for a
// single instance based on uptime × rate.
func instanceCostBreakdown(inst *db.Launch) (duration, actualCost string) {
	if inst.LaunchedAt == nil {
		return "—", "—"
	}
	var end time.Time
	if inst.EndedAt != nil {
		end = time.Unix(*inst.EndedAt, 0)
	} else {
		end = time.Now()
	}
	uptime := end.Sub(time.Unix(*inst.LaunchedAt, 0))
	hours := uptime.Hours()
	cents := hours * float64(inst.CostPerHourCents)

	h := int(uptime.Hours())
	m := int(uptime.Minutes()) % 60
	if h > 0 {
		duration = fmt.Sprintf("%dh%02dm", h, m)
	} else {
		duration = fmt.Sprintf("%dm", m)
	}
	actualCost = campaign.FormatCostCents(cents)
	return duration, actualCost
}
