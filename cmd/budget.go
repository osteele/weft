package cmd

import (
	"fmt"

	"github.com/osteele/weft/internal/vastai"
	"github.com/spf13/cobra"
)

var budgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Show Vast.ai account balance",
	Long:  `Display your Vast.ai credit balance and optionally open the billing page.`,
	Args:  cobra.NoArgs,
	RunE:  runBudget,
}

func init() {
	rootCmd.AddCommand(budgetCmd)
	budgetCmd.Flags().Bool("open", false, "Open the Vast.ai billing page in your browser")
}

func runBudget(cmd *cobra.Command, args []string) error {
	client := vastai.NewClient()
	user, err := client.ShowUser()
	if err != nil {
		return err
	}

	fmt.Printf("Vast.ai balance: $%.2f\n", user.Credit)

	open, _ := cmd.Flags().GetBool("open")
	if open {
		if err := openURL("https://cloud.vast.ai/billing/"); err != nil {
			return fmt.Errorf("open browser: %w", err)
		}
	}
	return nil
}
