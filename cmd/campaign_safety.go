package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var (
	campaignSafetyCampaignID int64
	campaignSafetyProject    string
)

var campaignSafetyCmd = &cobra.Command{
	Use:   "safety",
	Short: "Manage campaign auto-safety controls",
}

var campaignSafetyResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume auto-relaunch after a runaway breaker trip",
	RunE: func(cmd *cobra.Command, args []string) error {
		if campaignSafetyCampaignID <= 0 {
			return fmt.Errorf("--campaign must be > 0")
		}
		database, err := db.Open()
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()

		project := strings.TrimSpace(campaignSafetyProject)
		if project == "" {
			project = "<all>"
		}
		detail := fmt.Sprintf("project=%s; manual resume via CLI", strings.ReplaceAll(project, ";", "_"))
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind:  db.EventRelaunchRunawayResumed,
			CampaignID: campaignSafetyCampaignID,
			Detail:     detail,
		}); err != nil {
			return fmt.Errorf("record resume event: %w", err)
		}
		fmt.Printf("Resumed auto-relaunch safety breaker for campaign %d (project scope: %s)\n", campaignSafetyCampaignID, project)
		return nil
	},
}

func init() {
	campaignCmd.AddCommand(campaignSafetyCmd)
	campaignSafetyCmd.AddCommand(campaignSafetyResumeCmd)

	campaignSafetyResumeCmd.Flags().Int64Var(&campaignSafetyCampaignID, "campaign", 0, "Campaign ID to resume")
	campaignSafetyResumeCmd.Flags().StringVar(&campaignSafetyProject, "project", "", "Project scope (default: all projects in campaign)")
}
