package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var (
	providerDiagnoseStartupElapsed    time.Duration
	providerDiagnoseStartupDataCenter string
	providerDiagnoseStartupJSON       bool
)

var providerDiagnoseStartupCmd = &cobra.Command{
	Use:   "diagnose-startup PROVIDER PROVIDER_INSTANCE_ID",
	Short: "Diagnose startup risk for a direct provider instance",
	Long: `Diagnose startup risk for a direct provider instance that is not tracked as
a Weft wi... instance. This is read-only: it fetches current provider status
when possible and applies Weft's historical bootstrap/first-registration
survival model for the provider and data center.`,
	Args: usageArgs(cobra.ExactArgs(2)),
	RunE: runProviderDiagnoseStartup,
}

type providerStartupDiagnosis struct {
	Provider           string              `json:"provider"`
	ProviderInstanceID string              `json:"provider_instance_id"`
	ProviderStatus     string              `json:"provider_status,omitempty"`
	IntendedStatus     string              `json:"intended_status,omitempty"`
	StatusMessage      string              `json:"status_message,omitempty"`
	DataCenter         string              `json:"data_center,omitempty"`
	MachineID          string              `json:"machine_id,omitempty"`
	ElapsedSeconds     *int64              `json:"elapsed_seconds,omitempty"`
	Bootstrap          startupSurvivalView `json:"bootstrap"`
	FirstRegistration  startupSurvivalView `json:"first_registration"`
	Recommendation     string              `json:"recommendation"`
	Notes              []string            `json:"notes,omitempty"`
	ProviderError      string              `json:"provider_error,omitempty"`
}

type startupSurvivalView struct {
	Scope                 string  `json:"scope"`
	Samples               int     `json:"samples"`
	WarnAfterSeconds      int64   `json:"warn_after_seconds"`
	TerminateAfterSeconds int64   `json:"terminate_after_seconds"`
	ConditionalSuccess    *string `json:"conditional_success,omitempty"`
	RemainingMedian       *string `json:"remaining_median,omitempty"`
}

func init() {
	providerCmd.AddCommand(providerDiagnoseStartupCmd)
	providerDiagnoseStartupCmd.Flags().DurationVar(&providerDiagnoseStartupElapsed, "elapsed", 0, "Elapsed time since provider instance creation/start (for example 5m34s)")
	providerDiagnoseStartupCmd.Flags().StringVar(&providerDiagnoseStartupDataCenter, "data-center", "", "Provider data center/region for scoped first-registration history")
	providerDiagnoseStartupCmd.Flags().BoolVar(&providerDiagnoseStartupJSON, "json", false, "Print diagnosis as JSON")
}

func runProviderDiagnoseStartup(cmd *cobra.Command, args []string) error {
	provider, err := parseConfigProvider(args[0])
	if err != nil {
		return err
	}
	providerInstanceID := strings.TrimSpace(args[1])
	if providerInstanceID == "" {
		return usageErrorf("provider instance ID is required")
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var inst *cloud.Instance
	var providerErr error
	if client := providerDiagnoseStartupClient(cfg, provider); client != nil {
		inst, providerErr = client.ShowInstance(providerInstanceID)
	} else {
		providerErr = fmt.Errorf("no cloud client configured for %s", provider)
	}

	report := buildProviderStartupDiagnosis(database, provider, providerInstanceID, inst, providerErr, providerDiagnoseStartupDataCenter, providerDiagnoseStartupElapsed)
	if providerDiagnoseStartupJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Fprint(cmd.OutOrStdout(), formatProviderStartupDiagnosis(report))
	return nil
}

func providerDiagnoseStartupClient(cfg *config.Config, provider cloud.Provider) cloud.Client {
	clients, err := providerOfferClients(cfg, string(provider), "")
	if err != nil || len(clients) == 0 {
		return nil
	}
	return cloudClientForProvider(clients, provider)
}

func buildProviderStartupDiagnosis(database *sql.DB, provider cloud.Provider, providerInstanceID string, inst *cloud.Instance, providerErr error, dataCenter string, elapsed time.Duration) providerStartupDiagnosis {
	dataCenter = strings.TrimSpace(dataCenter)
	if inst != nil && dataCenter == "" {
		dataCenter = strings.TrimSpace(inst.DataCenter)
	}

	var elapsedSeconds *int64
	if elapsed > 0 {
		seconds := int64(elapsed.Round(time.Second) / time.Second)
		elapsedSeconds = &seconds
	}

	bootstrap, _ := db.ComputeBootstrapSurvival(database, string(provider))
	firstReg, _ := db.ComputeFirstRegistrationSurvival(database, db.FirstRegistrationScope{
		Provider:   string(provider),
		DataCenter: dataCenter,
	})

	report := providerStartupDiagnosis{
		Provider:           string(provider),
		ProviderInstanceID: providerInstanceID,
		DataCenter:         dataCenter,
		ElapsedSeconds:     elapsedSeconds,
		Bootstrap:          bootstrapStartupView(bootstrap, elapsed),
		FirstRegistration:  firstRegistrationStartupView(firstReg, elapsed),
		Recommendation:     startupRecommendation(elapsed, bootstrap, firstReg),
	}
	if inst != nil {
		report.ProviderStatus = inst.Status
		report.IntendedStatus = inst.IntendedStatus
		report.StatusMessage = inst.StatusMsg
		report.MachineID = inst.MachineID
		if report.DataCenter == "" {
			report.DataCenter = inst.DataCenter
		}
	}
	if providerErr != nil {
		report.ProviderError = providerErr.Error()
		report.Notes = append(report.Notes, "provider lookup failed; recommendation uses historical Weft data only")
	}
	if elapsed <= 0 {
		report.Notes = append(report.Notes, "pass --elapsed to compare this instance against learned startup thresholds")
	}
	if dataCenter == "" {
		report.Notes = append(report.Notes, "no data center supplied by provider; first-registration history used provider/global fallback")
	}
	return report
}

func bootstrapStartupView(s *db.BootstrapSurvival, elapsed time.Duration) startupSurvivalView {
	if s == nil {
		return startupSurvivalView{Scope: "provider"}
	}
	view := startupSurvivalView{
		Scope:                 s.Provider,
		Samples:               s.SampleSize,
		WarnAfterSeconds:      int64(s.WarnAfter / time.Second),
		TerminateAfterSeconds: int64(s.TerminateAfter / time.Second),
	}
	if elapsed > 0 {
		if remaining, ok := s.Durations.ConditionalMedian(elapsed); ok {
			value := remaining.Truncate(time.Second).String()
			view.RemainingMedian = &value
		}
	}
	return view
}

func firstRegistrationStartupView(s *db.FirstRegistrationSurvival, elapsed time.Duration) startupSurvivalView {
	if s == nil {
		return startupSurvivalView{Scope: db.FirstRegistrationScopeGlobal}
	}
	view := startupSurvivalView{
		Scope:                 s.ScopeDescription(),
		Samples:               s.SampleSize,
		WarnAfterSeconds:      int64(s.WarnAfter / time.Second),
		TerminateAfterSeconds: int64(s.TerminateAfter / time.Second),
	}
	if elapsed > 0 {
		if p, ok := s.ConditionalSuccess(elapsed); ok {
			value := fmt.Sprintf("%.0f%%", p*100)
			view.ConditionalSuccess = &value
		}
		if remaining, ok := s.Durations.ConditionalMedian(elapsed); ok {
			value := remaining.Truncate(time.Second).String()
			view.RemainingMedian = &value
		}
	}
	return view
}

func startupRecommendation(elapsed time.Duration, bootstrap *db.BootstrapSurvival, firstReg *db.FirstRegistrationSurvival) string {
	if elapsed <= 0 {
		return "status only"
	}
	terminate := time.Duration(0)
	warn := time.Duration(0)
	if bootstrap != nil {
		terminate = maxDuration(terminate, bootstrap.TerminateAfter)
		warn = maxDuration(warn, bootstrap.WarnAfter)
	}
	if firstReg != nil {
		terminate = maxDuration(terminate, firstReg.TerminateAfter)
		warn = maxDuration(warn, firstReg.WarnAfter)
	}
	if terminate > 0 && elapsed >= terminate {
		return "terminate or replace: elapsed time exceeds learned startup terminate threshold"
	}
	if warn > 0 && elapsed >= warn {
		return "watch closely: elapsed time exceeds learned startup warning threshold"
	}
	return "wait: elapsed time is within learned startup thresholds"
}

func maxDuration(a, b time.Duration) time.Duration {
	if b > a {
		return b
	}
	return a
}

func formatProviderStartupDiagnosis(report providerStartupDiagnosis) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Provider instance %s:%s\n", report.Provider, report.ProviderInstanceID)
	if report.ProviderStatus != "" {
		fmt.Fprintf(&b, "  Status:     %s\n", report.ProviderStatus)
	}
	if report.IntendedStatus != "" {
		fmt.Fprintf(&b, "  Intended:   %s\n", report.IntendedStatus)
	}
	if report.StatusMessage != "" {
		fmt.Fprintf(&b, "  Message:    %s\n", report.StatusMessage)
	}
	if report.DataCenter != "" {
		fmt.Fprintf(&b, "  Location:   %s\n", report.DataCenter)
	}
	if report.MachineID != "" {
		fmt.Fprintf(&b, "  Machine:    %s\n", report.MachineID)
	}
	if report.ElapsedSeconds != nil {
		fmt.Fprintf(&b, "  Elapsed:    %s\n", (time.Duration(*report.ElapsedSeconds) * time.Second).String())
	}
	if report.ProviderError != "" {
		fmt.Fprintf(&b, "  Lookup:     %s\n", report.ProviderError)
	}
	fmt.Fprintf(&b, "  Recommend:  %s\n", report.Recommendation)

	b.WriteString("\nHistorical Startup:\n")
	formatStartupView(&b, "Bootstrap", report.Bootstrap)
	formatStartupView(&b, "First registration", report.FirstRegistration)
	for _, note := range report.Notes {
		fmt.Fprintf(&b, "  Note:       %s\n", note)
	}
	return b.String()
}

func formatStartupView(b *strings.Builder, label string, view startupSurvivalView) {
	fmt.Fprintf(b, "  %s: n=%d, scope=%s, warn after %s, terminate after %s\n",
		label,
		view.Samples,
		view.Scope,
		(time.Duration(view.WarnAfterSeconds) * time.Second).String(),
		(time.Duration(view.TerminateAfterSeconds) * time.Second).String(),
	)
	if view.ConditionalSuccess != nil {
		fmt.Fprintf(b, "    Conditional success: %s\n", *view.ConditionalSuccess)
	}
	if view.RemainingMedian != nil {
		fmt.Fprintf(b, "    Median remaining:    %s\n", *view.RemainingMedian)
	}
}
