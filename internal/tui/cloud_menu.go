package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/vastai"
)

// handleCloudMenuKeyPress handles key events when the cloud menu overlay is active.
func (m Model) handleCloudMenuKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Cost confirmation sub-state
	if m.cloudMenuConfirm {
		switch {
		case key.Matches(msg, keys.Enter):
			// Confirm: launch cloud job
			m.cloudMenuConfirm = false
			m.showCloudMenu = false
			offering := m.cloudMenuOfferings[m.cloudMenuCursor]
			return m, m.launchCloudJob(m.cloudMenuJob, offering)
		case key.Matches(msg, keys.Escape):
			// Cancel confirmation, go back to menu
			m.cloudMenuConfirm = false
			return m, nil
		}
		return m, nil
	}

	switch {
	case key.Matches(msg, keys.Escape):
		m.showCloudMenu = false
		m.cloudMenuJob = nil
		m.cloudMenuOfferings = nil
		m.cloudMenuLoading = false
		return m, nil

	case key.Matches(msg, keys.Up):
		if m.cloudMenuCursor > 0 {
			m.cloudMenuCursor--
		}
		return m, nil

	case key.Matches(msg, keys.Down):
		if m.cloudMenuCursor < len(m.cloudMenuOfferings)-1 {
			m.cloudMenuCursor++
		}
		return m, nil

	case key.Matches(msg, keys.Enter):
		if m.cloudMenuLoading || len(m.cloudMenuOfferings) == 0 {
			return m, nil
		}
		offering := m.cloudMenuOfferings[m.cloudMenuCursor]
		if offering.Source == "local" {
			// "Keep waiting" — just dismiss the menu
			m.showCloudMenu = false
			return m, nil
		}
		// Show cost confirmation
		m.cloudMenuConfirm = true
		return m, nil
	}

	return m, nil
}

// renderCloudMenu renders the cloud GPU offering overlay.
func (m Model) renderCloudMenu(background string) string {
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Width(66)

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
	selectedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))

	var b strings.Builder
	b.WriteString(titleStyle.Render("Send to Cloud GPU"))
	b.WriteString("\n\n")

	if m.cloudMenuLoading {
		b.WriteString(m.spinner.View())
		b.WriteString(" Searching for GPU offers...")
	} else if len(m.cloudMenuOfferings) == 0 {
		b.WriteString(dimStyle.Render("No offers available"))
	} else if m.cloudMenuConfirm {
		// Cost confirmation view
		offering := m.cloudMenuOfferings[m.cloudMenuCursor]
		b.WriteString(fmt.Sprintf("Estimated cost: ~$%.2f\n", offering.EstTotalCost))
		b.WriteString(fmt.Sprintf("GPU: %s %.0fGB @ $%.2f/hr\n", offering.GPUName, offering.GPUMemGB, offering.CostPerHour))
		b.WriteString(fmt.Sprintf("Setup: ~%.0fm + Run: ~%.0fm\n\n", offering.EstSetupMin, offering.EstRunMin))
		b.WriteString(selectedStyle.Render("Press Enter to confirm, Esc to cancel"))
	} else {
		for i, off := range m.cloudMenuOfferings {
			cursor := "  "
			style := normalStyle
			if i == m.cloudMenuCursor {
				cursor = "> "
				style = selectedStyle
			}
			b.WriteString(style.Render(cursor + off.DisplayName))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  \u2191/\u2193 navigate  Enter select  Esc cancel"))
	}

	modal := modalStyle.Render(b.String())
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		modal,
	)
}

// openCloudMenu opens the cloud menu for the selected job.
func (m *Model) openCloudMenu(job *db.Job) tea.Cmd {
	m.showCloudMenu = true
	m.cloudMenuJob = job
	m.cloudMenuCursor = 0
	m.cloudMenuLoading = true
	m.cloudMenuConfirm = false
	m.cloudMenuOfferings = nil

	return m.fetchCloudOffers(job)
}

// fetchCloudOffers fetches Vast.ai offers and builds the offerings list.
func (m *Model) fetchCloudOffers(job *db.Job) tea.Cmd {
	return func() tea.Msg {
		client := vastai.NewClient()

		// Check if vastai CLI is available
		if err := client.Available(); err != nil {
			return cloudOffersLoadedMsg{
				job: job,
				err: fmt.Errorf("Vast.ai not available: %w", err),
			}
		}

		// Build constraints from job metadata
		constraints := vastai.OfferConstraints{
			MinReliability: 0.95,
			NumGPUs:        1,
		}
		if job.GPUMemGB != nil {
			constraints.MinGPUMemGB = *job.GPUMemGB
		}
		if job.GPUClass != "" {
			constraints.GPUClass = job.GPUClass
		}

		// Search for offers
		offers, err := client.SearchOffers(constraints)
		if err != nil {
			return cloudOffersLoadedMsg{
				job: job,
				err: err,
			}
		}

		// Limit to top 5 by cost
		if len(offers) > 5 {
			offers = offers[:5]
		}

		// Build offerings with local option
		// Estimate local queue: count queued jobs ahead of this one on the same host
		queueDepth := 0
		avgJobMin := 15.0 // default estimate

		offerings := placement.BuildCloudOfferings(
			"local GPU", // will be overridden by host spec if available
			24.0,        // default GPU mem
			queueDepth,
			avgJobMin,
			0, // no DLPerf data for local
			offers,
		)

		return cloudOffersLoadedMsg{
			job:       job,
			offerings: offerings,
		}
	}
}

// launchCloudJob creates a Vast.ai instance and launches the job in fire-and-forget mode.
// The instance will upload results to R2 and self-destruct when done.
func (m *Model) launchCloudJob(job *db.Job, offering placement.CloudOffering) tea.Cmd {
	return func() tea.Msg {
		if offering.Offer == nil {
			return cloudJobLaunchedMsg{
				jobID: job.ID,
				err:   fmt.Errorf("no offer data"),
			}
		}

		// Check R2 config
		r2Cfg := m.appConfig.Vastai.R2
		if r2Cfg.Bucket == "" || r2Cfg.AccessKeyID == "" {
			return cloudJobLaunchedMsg{
				jobID: job.ID,
				err:   fmt.Errorf("R2 not configured (set vastai.r2 in config.yaml)"),
			}
		}

		client := vastai.NewClient()
		offer := *offering.Offer

		image := m.appConfig.Vastai.DefaultImage
		if image == "" {
			image = "nvidia/cuda:12.2-devel-ubuntu22.04"
		}

		opts := vastai.CreateOpts{
			Image:      image,
			DiskGB:     50,
			SSHEnabled: true,
			OnStartCmd: "curl -LsSf https://astral.sh/uv/install.sh | sh && curl https://rclone.org/install.sh | bash",
		}

		progress := func(phase string) {
			_ = phase
		}

		vastR2 := vastai.R2Config{
			AccountID:       r2Cfg.AccountID,
			AccessKeyID:     r2Cfg.AccessKeyID,
			SecretAccessKey: r2Cfg.SecretAccessKey,
			Bucket:          r2Cfg.Bucket,
		}

		result, err := vastai.LaunchJobOnInstance(client, offer, opts, job.WorkingDir, job.Command, job.Inputs, job.ID, vastR2, progress)
		if err != nil {
			return cloudJobLaunchedMsg{
				jobID: job.ID,
				err:   err,
			}
		}

		// Persist instance ID to DB immediately
		if dbErr := db.SetJobVastaiInstance(m.database, job.ID, result.InstanceID); dbErr != nil {
			// Instance is already running — log but don't fail
			_ = dbErr
		}

		return cloudJobLaunchedMsg{
			jobID:      job.ID,
			instanceID: result.InstanceID,
		}
	}
}
