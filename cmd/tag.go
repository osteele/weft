package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var tagCmd = &cobra.Command{
	Use:   "tag",
	Short: "Manage job tags",
	Long: `Manage job tags stored in the local database.

Tags are free-form labels that help group jobs and track processing state.
Most tags are user-defined, but the following have special meaning to the
scheduler and runner:

  processed         Marks a job as processed; filtered by --processed /
                    --unprocessed on 'weft jobs list'. Set with
                    'weft mark-processed'.
  benchmark         Treat the job as a benchmark: requires an idle host,
                    enables strict GPU isolation, optional GPU warmup, and
                    blocks combination with 'interruptible'.
  exclusive         Requires the host to be idle while the job runs (like
                    'benchmark', without the timing-protection requirements).
  compute-intensive Job saturates the GPU regardless of co-tenants. Skips the
                    queue-contention penalty in run-time estimation and adds a
                    placement bonus proportional to the host's free CPU
                    capacity (cores × cpu_factor × idle fraction).
  rental            Bind the job to a cloud rental instance (alias: 'cloud').
  inventory         Bind the job to an on-prem inventory host (alias:
                    'on-prem').
  interruptible     Job is preemptible / safe to run on spot instances
                    (alias: 'preemptible').
  provider:<name>   Pin to a specific cloud provider, e.g. 'provider:vastai'
                    or 'provider:runpod'.

Legacy aliases ('cloud', 'on-prem', 'preemptible') are accepted on input and
canonicalized to the names above.`,
}

var tagAddCmd = &cobra.Command{
	Use:   "add <job-id>... <tag>",
	Short: "Add a tag to a job",
	Args:  usageArgs(cobra.MinimumNArgs(2)),
	RunE:  runTagAdd,
}

var tagRemoveCmd = &cobra.Command{
	Use:     "rm <job-id>... <tag>",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a tag from a job",
	Args:    usageArgs(cobra.MinimumNArgs(2)),
	RunE:    runTagRemove,
}

func init() {
	rootCmd.AddCommand(tagCmd)
	tagCmd.AddCommand(tagAddCmd)
	tagCmd.AddCommand(tagRemoveCmd)
}

func runTagAdd(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDsWithExplicitPrefix(args[:len(args)-1])
	if err != nil {
		return err
	}
	tag := args[len(args)-1]
	displayTag := db.CanonicalizeTag(tag)

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errorsList []string
	for _, jobID := range jobIDs {
		if err := db.AddJobTag(database, jobID, tag); err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		fmt.Printf("Added tag %q to job %s\n", displayTag, ids.FormatJobID(jobID))
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: reload: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if job != nil && job.HasTagHostConflict() {
			oldHost := job.Host
			if result, err := ops.UnplaceQueuedJob(database, job, ops.DefaultOptions()); err != nil {
				errorsList = append(errorsList, fmt.Sprintf("job %s: unplace: %v", ids.FormatJobID(jobID), err))
			} else if result.Success {
				fmt.Printf("  Unplaced job %s from %s (rental tag conflicts with inventory host)\n", ids.FormatJobID(jobID), oldHost)
			}
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func runTagRemove(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDsWithExplicitPrefix(args[:len(args)-1])
	if err != nil {
		return err
	}
	tag := args[len(args)-1]
	displayTag := db.CanonicalizeTag(tag)

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errorsList []string
	for _, jobID := range jobIDs {
		if err := db.RemoveJobTag(database, jobID, tag); err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		fmt.Printf("Removed tag %q from job %s\n", displayTag, ids.FormatJobID(jobID))
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}
