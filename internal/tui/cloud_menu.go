package tui

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
	weftsync "github.com/osteele/weft/internal/sync"
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

// renderCloudMenu renders the rental GPU offering overlay.
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
	b.WriteString(titleStyle.Render("Send to Rental GPU"))
	b.WriteString("\n\n")

	if m.cloudMenuLoading {
		b.WriteString(m.spinner.View())
		b.WriteString(" Searching for rental GPU offers...")
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

// openCloudMenu opens the rental menu for the selected job.
func (m *Model) openCloudMenu(job *db.Job) tea.Cmd {
	if job != nil && job.HasTag(db.TagInventory) {
		return m.setFlash("Inventory-only jobs cannot launch on rental GPUs", true)
	}
	m.showCloudMenu = true
	m.cloudMenuJob = job
	m.cloudMenuCursor = 0
	m.cloudMenuLoading = true
	m.cloudMenuConfirm = false
	m.cloudMenuOfferings = nil

	return m.fetchCloudOffers(job)
}

// fetchCloudOffers fetches cloud offers from all enabled providers and builds the offerings list.
func (m *Model) fetchCloudOffers(job *db.Job) tea.Cmd {
	clients := m.cloudClients
	pending := m.cloudDiscoveryPending
	return func() tea.Msg {
		if pending {
			return cloudOffersLoadedMsg{
				job: job,
				err: fmt.Errorf("rental providers still initializing"),
			}
		}
		if len(clients) == 0 {
			err := m.cloudClientErr
			if err == nil {
				err = fmt.Errorf("no rental providers available")
			}
			return cloudOffersLoadedMsg{
				job: job,
				err: err,
			}
		}

		// Build constraints from job metadata
		constraints := cloud.OfferConstraints{
			MinReliability: cloud.DefaultMinReliability,
			NumGPUs:        1,
		}
		if job.GPUMemGB != nil {
			constraints.MinGPUMemGB = *job.GPUMemGB
		}
		if job.GPUClass != "" {
			constraints.GPUClass = job.GPUClass
		}

		// Search for offers across all providers
		offers, err := cloud.SearchAllProviders(clients, constraints)
		if err != nil {
			return cloudOffersLoadedMsg{
				job: job,
				err: err,
			}
		}

		// Sort and limit to top 5 by cost
		cloud.SortOffersByCost(offers)
		if len(offers) > 5 {
			offers = offers[:5]
		}

		// Build offerings with local option
		queueDepth := 0
		avgJobMin := 15.0

		offerings := placement.BuildCloudOfferings(
			"local GPU",
			24.0,
			queueDepth,
			avgJobMin,
			0,
			offers,
		)

		return cloudOffersLoadedMsg{
			job:       job,
			offerings: offerings,
		}
	}
}

// launchCloudJob creates a campaign for a single job, launches it on a cloud provider,
// and tracks it through the campaign system.
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
				err:   fmt.Errorf("R2 not configured (set vastai.r2 in config.toml)"),
			}
		}

		offer := *offering.Offer

		// Find the right client for this offer's provider
		var client cloud.Client
		for _, c := range m.cloudClients {
			if c.Provider() == offer.Provider {
				client = c
				break
			}
		}
		if client == nil && len(m.cloudClients) > 0 {
			client = m.cloudClients[0]
		}
		if client == nil {
			return cloudJobLaunchedMsg{
				jobID: job.ID,
				err:   fmt.Errorf("no cloud client for provider %s", offer.Provider),
			}
		}

		createOpts, err := m.appConfig.CloudCreateOpts(offer.Provider)
		if err != nil {
			return cloudJobLaunchedMsg{jobID: job.ID, err: err}
		}

		cloudR2 := r2Cfg.ToCloudR2Config()

		gpuMemGB := 0
		if job.GPUMemGB != nil {
			gpuMemGB = *job.GPUMemGB
		}
		group := campaign.InstanceGroup{
			GPUClass: job.GPUClass,
			GPUMemGB: gpuMemGB,
			Jobs:     []*db.Job{job},
		}

		agentVer, err := agentdeploy.LocalAgentVersion()
		if err != nil {
			return cloudJobLaunchedMsg{jobID: job.ID, err: fmt.Errorf("local agent version: %w", err)}
		}

		// Create R2 client and pre-stage assets
		r2Client, err := r2.New(r2.Config{
			AccountID:       cloudR2.AccountID,
			AccessKeyID:     cloudR2.AccessKeyID,
			SecretAccessKey: cloudR2.SecretAccessKey,
			Bucket:          cloudR2.Bucket,
		})
		if err != nil {
			return cloudJobLaunchedMsg{jobID: job.ID, err: fmt.Errorf("create R2 client: %w", err)}
		}

		ctx := context.Background()
		agentR2Key, err := agentdeploy.EnsureAgentInR2(ctx, r2Client, agentVer, "linux", "amd64")
		if err != nil {
			return cloudJobLaunchedMsg{jobID: job.ID, err: fmt.Errorf("upload agent: %w", err)}
		}

		sourceR2Keys := make(map[string]string)
		for _, d := range group.SourceDirs() {
			key, err := weftsync.UploadSourceToR2(ctx, r2Client, d)
			if err != nil {
				return cloudJobLaunchedMsg{jobID: job.ID, err: fmt.Errorf("upload source: %w", err)}
			}
			sourceR2Keys[d] = key
		}

		campaignID, err := campaign.LaunchInstance(
			client, m.database, nil, group, offer, campaign.LaunchOpts{}, cloudR2, createOpts,
			campaign.R2Assets{Client: r2Client, AgentR2Key: agentR2Key, SourceR2Keys: sourceR2Keys},
			nil,
			func(phase string) {
				log.Printf("cloud: job %d instance: %s", job.ID, phase)
			},
			nil,
		)
		if err != nil {
			return cloudJobLaunchedMsg{
				jobID: job.ID,
				err:   err,
			}
		}

		return cloudJobLaunchedMsg{
			jobID:      job.ID,
			instanceID: int(campaignID),
		}
	}
}
