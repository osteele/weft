package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/spf13/cobra"
)

var providerListJSON bool

var providerCmd = &cobra.Command{
	Use:     "provider",
	Aliases: []string{"providers"},
	Short:   "Manage cloud provider enablement",
}

var providerListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"status"},
	Short:   "List cloud provider configuration",
	Args:    cobra.NoArgs,
	RunE:    runProviderList,
}

var providerEnableCmd = &cobra.Command{
	Use:   "enable PROVIDER",
	Short: "Enable a cloud provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProviderSet(cmd, args[0], config.Bool(true), "enabled")
	},
}

var providerDisableCmd = &cobra.Command{
	Use:   "disable PROVIDER",
	Short: "Disable a cloud provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProviderSet(cmd, args[0], config.Bool(false), "disabled")
	},
}

var providerResetCmd = &cobra.Command{
	Use:   "reset PROVIDER",
	Short: "Reset a cloud provider to the default policy",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProviderSet(cmd, args[0], nil, "reset to auto")
	},
}

type providerStatusView struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Config   string `json:"config"`
	Active   bool   `json:"active"`
	Note     string `json:"note,omitempty"`
}

func init() {
	rootCmd.AddCommand(providerCmd)
	providerCmd.AddCommand(providerListCmd)
	providerCmd.AddCommand(providerEnableCmd)
	providerCmd.AddCommand(providerDisableCmd)
	providerCmd.AddCommand(providerResetCmd)
	providerListCmd.Flags().BoolVar(&providerListJSON, "json", false, "Print provider status as JSON")
}

func runProviderList(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	rows := providerStatusRows(cfg)
	if providerListJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tCONFIG\tACTIVE\tNOTE")
	for _, row := range rows {
		active := "no"
		if row.Active {
			active = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", row.Provider, row.Config, active, row.Note)
	}
	return w.Flush()
}

func runProviderSet(cmd *cobra.Command, name string, enabled *bool, action string) error {
	provider, err := parseConfigProvider(name)
	if err != nil {
		return err
	}
	if err := config.SetProviderEnabledSetting(provider, enabled); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s in %s\n", provider, action, config.ConfigPath())
	return nil
}

func providerStatusRows(cfg *config.Config) []providerStatusView {
	providers := []cloud.Provider{cloud.ProviderVastai, cloud.ProviderRunpod}
	rows := make([]providerStatusView, 0, len(providers))
	anyExplicit := cfg != nil && cfg.AnyProviderExplicitlyConfigured()
	for _, provider := range providers {
		setting, _ := cfg.ProviderEnabledSetting(provider)
		configState := "auto"
		if setting != nil {
			if *setting {
				configState = "enabled"
			} else {
				configState = "disabled"
			}
		}
		note := ""
		if setting == nil && provider == cloud.ProviderVastai && !anyExplicit {
			note = "legacy default"
		} else if setting == nil && anyExplicit {
			note = "inactive while providers are explicit"
		}
		rows = append(rows, providerStatusView{
			Provider: string(provider),
			Name:     provider.DisplayName(),
			Config:   configState,
			Active:   cfg.ProviderEnabledForDiscovery(provider),
			Note:     note,
		})
	}
	return rows
}

func parseConfigProvider(name string) (cloud.Provider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case string(cloud.ProviderVastai), "vast", "vast.ai":
		return cloud.ProviderVastai, nil
	case string(cloud.ProviderRunpod):
		return cloud.ProviderRunpod, nil
	default:
		return "", fmt.Errorf("unknown provider %q (expected vastai or runpod)", name)
	}
}
