package terminal

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/r2"
)

// Aliases for shared TUI styles used in the launch TUI.
var (
	launchTitleStyle    = tuiTitleStyle
	launchHeaderStyle   = lipgloss.NewStyle().Bold(true)
	launchSelectedStyle = lipgloss.NewStyle()
	launchDimStyle      = tuiDimStyle
	launchNoOfferStyle  = lipgloss.NewStyle().Foreground(tuiCompletedColor).Strikethrough(true)
	launchErrStyle      = tuiFailedStyle
)

const (
	launchHeartbeatInterval = 2 * time.Second
	launchStallThreshold    = 20 * time.Second
)

// renderRow writes a styled line with cursor prefix to b.
func renderRow(b *strings.Builder, style lipgloss.Style, line string, isCursor bool) {
	if isCursor {
		style = style.Bold(true)
	}
	prefix := "  "
	if isCursor {
		prefix = "> "
	}
	b.WriteString(style.Render(prefix + line))
}

// formatPartialErrors renders a list of launch failure messages, wrapping long
// lines so the full error remains readable in narrow terminals.
func formatPartialErrors(errors []string, width int) string {
	var b strings.Builder
	header := fmt.Sprintf("%d planned launch(es) failed:", len(errors))
	if width > 0 {
		header = truncateDisplayWidth(header, width)
	}
	b.WriteString(launchErrStyle.Render(header))
	for _, errMsg := range errors {
		b.WriteString("\n")
		for i, line := range wrapPrefixedText(errMsg, width, "  - ", "    ") {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(line)
		}
	}
	return b.String()
}

func wrapPrefixedText(text string, width int, firstPrefix string, continuationPrefix string) []string {
	if width <= 0 {
		return []string{firstPrefix + text}
	}
	available := width - lipgloss.Width(firstPrefix)
	if available < 8 {
		return []string{firstPrefix + truncateDisplayWidth(text, max(width-lipgloss.Width(firstPrefix), 1))}
	}

	wrapped := wrapDisplayWidth(text, available)
	lines := make([]string, 0, len(wrapped))
	for i, line := range wrapped {
		prefix := continuationPrefix
		if i == 0 {
			prefix = firstPrefix
		}
		lines = append(lines, prefix+line)
	}
	return lines
}

func wrapDisplayWidth(text string, width int) []string {
	if width <= 0 || lipgloss.Width(text) <= width {
		return []string{text}
	}

	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}

	lines := make([]string, 0, 4)
	current := ""
	currentWidth := 0

	flush := func() {
		if current == "" {
			return
		}
		lines = append(lines, current)
		current = ""
		currentWidth = 0
	}

	for _, word := range words {
		segments := splitTokenByDisplayWidth(word, width)
		for idx, segment := range segments {
			segmentWidth := lipgloss.Width(segment)
			if current == "" {
				current = segment
				currentWidth = segmentWidth
				continue
			}
			if idx == 0 && currentWidth+1+segmentWidth <= width {
				current += " " + segment
				currentWidth += 1 + segmentWidth
				continue
			}
			flush()
			current = segment
			currentWidth = segmentWidth
		}
	}
	flush()
	return lines
}

func splitTokenByDisplayWidth(token string, width int) []string {
	if width <= 0 || lipgloss.Width(token) <= width {
		return []string{token}
	}

	parts := make([]string, 0, lipgloss.Width(token)/width+1)
	var b strings.Builder
	currentWidth := 0
	for _, r := range token {
		runeWidth := lipgloss.Width(string(r))
		if currentWidth+runeWidth > width && b.Len() > 0 {
			parts = append(parts, b.String())
			b.Reset()
			currentWidth = 0
		}
		b.WriteRune(r)
		currentWidth += runeWidth
		if currentWidth >= width {
			parts = append(parts, b.String())
			b.Reset()
			currentWidth = 0
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

func formatTradeoffColumnHeader(table campaign.CostTable) string {
	strategyWidth := max(1, table.TimeColOffset-4)
	timeHeader := truncateDisplayWidth("duration", max(table.TimeWidth, 1))
	rateHeader := truncateDisplayWidth("burn", max(table.RateWidth, 1))
	return fmt.Sprintf("  %-*s  %-*s  %-*s  %s  %s",
		strategyWidth, "option",
		max(table.TimeWidth, len(timeHeader)), timeHeader,
		max(table.RateWidth, len(rateHeader)), rateHeader,
		"total cost",
		"instances",
	)
}

// listItem is a union type for the flat list of items in the launch selector.
type listItem struct {
	isHeader bool
	groupIdx int    // index into groups/groupOffers
	label    string // header text or job line
	jobID    int64  // only for job rows
}

// launchFocusArea tracks which section of the TUI has cursor focus.
type launchFocusArea int

const (
	focusJobs      launchFocusArea = iota // cursor is in the job list
	focusTradeoffs                        // cursor is in the tradeoff comparison
)

type launchModel struct {
	groups          []campaign.InstanceGroup
	groupOffers     []campaign.GroupOffer
	cachedRawOffers []campaign.GroupRawOffers // cached raw offers for the split (base) groups
	tradeoffPlans   map[string]campaign.StrategyPlan
	tradeoffOptions []campaign.TradeoffOption
	activeTradeoff  string
	costEstimates   []campaign.CostEstimate
	estimateCache   map[string][]campaign.CostEstimate // offerIdentity → estimates
	predConfig      *predictor.Config
	overheadModel   *estimate.OverheadModel
	survivalModel   *bidding.SurvivalModel

	items    []listItem
	cursor   int
	offset   int
	selected map[int64]bool // job ID -> checked

	showCostDetail    bool
	tradeoffDisclosed bool // true = show detail rows for active tradeoff inline

	// Focus state for unified jobs+strategies navigation.
	focusArea      launchFocusArea // which area has cursor focus
	tradeoffCursor int             // index into tradeoffOptions (persists when in job area)

	// winningCandidate caches the best candidate result for the active tradeoff.
	// It drives the display; launchInstances recomputes a fresh execution plan.
	winningCandidate *campaign.CandidateResult
	reuseAssignments []campaign.ReuseAssignment

	campaignID       int64
	reconciling      bool // true while background reconciliation is in progress
	loading          bool
	launching        bool
	done             bool
	err              error
	statusHint       string // transient hint shown below the cost table (cleared on next action)
	partialErrors    []string
	fromWatch        bool
	campaignPhase    string              // campaign-level phase text
	groupPhases      map[int]string      // groupIndex -> current phase
	groupDone        map[int]bool        // groupIndex -> registered
	estimateProgress estimateProgressMsg // latest estimation progress
	progressCh       chan estimateProgressMsg
	campaignCh       chan int64
	planCh           chan launchExecutionPlanMsg
	phaseCh          chan launchPhaseMsg
	instanceCh       chan launchInstanceRegisteredMsg
	assetStageCh     chan assetStageChangedMsg

	instanceIDs             []int64
	expectedInstanceCount   int
	launchStartedAt         time.Time
	lastLaunchProgressAt    time.Time
	launchStatusCounts      map[string]int
	launchHeartbeatErr      error
	inlineWatchEnabled      bool
	inlineWatchUsed         bool
	registeredInstanceIDs   []int64
	registeredInstanceIDSet map[int64]struct{}
	inlineWatch             *watchModel
	database                *sql.DB
	clients                 []cloud.Client
	reusable                []campaign.InstanceCapacity
	providerErr             error
	appConfig               *config.Config
	launchOpts              campaign.LaunchOpts
	gpuFilter               string // --gpu filter to reapply after reconciliation
	projectFilter           string // project scope when launching from project watch
	reconciler              *campaign.Reconciler
	reconcileDropped        int
	assetStageSignature     string
	assetStageErr           error
	assetStager             *campaign.R2AssetStager

	// Cached tradeoff display data — recomputed only when tradeoffRowsDirty.
	cachedTradeoffRows    []campaign.StrategySummaryRow
	cachedActiveEstimates []campaign.CostEstimate
	tradeoffRowsDirty     bool

	spinner spinner.Model
	width   int
	height  int
}

// Messages
type reconcileDoneMsg struct {
	groups []campaign.InstanceGroup
	err    error
	warn   string
}

type rawOffersLoadedMsg struct {
	raw        []campaign.GroupRawOffers
	reusable   []campaign.InstanceCapacity
	err        error
	background bool // true if this is a background refresh (don't show loading state)
}

type profilePlansLoadedMsg struct {
	plans      map[string]campaign.StrategyPlan
	options    []campaign.TradeoffOption
	err        error
	background bool // true if this is a background refresh (don't show loading state)
}

type hfPrefetchDoneMsg struct{} // no-op; side effect is warming the HF size cache

type estimatesLoadedMsg struct {
	estimates []campaign.CostEstimate
	cacheKey  string // offerIdentity key for cache storage
}

type estimateProgressMsg struct {
	phase           string // e.g., "Resolving model sizes", "Estimating job durations"
	resolved, total int
}

type instancesLaunchedMsg struct {
	instanceIDs []int64
	errors      []error
	err         error
}

type launchHeartbeatMsg struct {
	instanceIDs  []int64
	statusCounts map[string]int
	err          error
	polledAt     time.Time
}

type campaignCreatedMsg struct {
	campaignID int64
}

type launchPhaseMsg struct {
	groupIndex int // -1 for campaign-level phases
	phase      string
}

type launchExecutionPlanMsg struct {
	expectedInstanceCount int
}

type launchInstanceRegisteredMsg struct {
	instanceID int64
	groupIndex int
}

type assetStageStartedMsg struct {
	signature string
	stager    *campaign.R2AssetStager
	err       error
}

type assetStageChangedMsg struct {
	signature string
}

// offersMatch returns true if both slices select the same best offer per group.
// Uses provider+ID composite key to avoid false matches across providers.
func offersMatch(a, b []campaign.GroupOffer) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		aKey := offerKey(a[i].Offer)
		bKey := offerKey(b[i].Offer)
		if aKey != bKey {
			return false
		}
	}
	return true
}

func offerKey(o *cloud.Offer) string {
	if o == nil {
		return ""
	}
	return o.Key()
}

// offerIdentity returns a deterministic cache key for a set of group offers,
// based on provider+ID of each offer. Two strategies that select the same
// offers produce the same identity.
func offerIdentity(offers []campaign.GroupOffer) string {
	keys := make([]string, len(offers))
	for i, gOffer := range offers {
		keys[i] = offerKey(gOffer.Offer)
	}
	return strings.Join(keys, "|")
}

// groupsChanged returns true if the groups differ in count or job composition.
func groupsChanged(old, new []campaign.InstanceGroup) bool {
	if len(old) != len(new) {
		return true
	}
	for i := range old {
		if old[i].GPUClass != new[i].GPUClass || len(old[i].Jobs) != len(new[i].Jobs) {
			return true
		}
		for j := range old[i].Jobs {
			if old[i].Jobs[j].ID != new[i].Jobs[j].ID {
				return true
			}
		}
	}
	return false
}

// buildItemsFromGroups creates the flat item list, selection map, and initial
// cursor position from instance groups.
func buildItemsFromGroups(groups []campaign.InstanceGroup) ([]listItem, map[int64]bool, int) {
	var items []listItem
	selected := make(map[int64]bool)

	for i, g := range groups {
		items = append(items, listItem{
			isHeader: true,
			groupIdx: i,
			label:    fmt.Sprintf("%s (%s)", g.GPUSpec(), campaign.PluralJobs(len(g.Jobs))),
		})
		for _, job := range g.Jobs {
			items = append(items, listItem{
				groupIdx: i,
				jobID:    job.ID,
				label:    campaign.FormatJobLine(job),
			})
			selected[job.ID] = true
		}
	}

	cursor := 0
	for i, item := range items {
		if !item.isHeader {
			cursor = i
			break
		}
	}
	return items, selected, cursor
}

func countGroupJobs(groups []campaign.InstanceGroup) int {
	n := 0
	for _, g := range groups {
		n += len(g.Jobs)
	}
	return n
}

func launchAssetStageSignature(groups []campaign.InstanceGroup) string {
	seen := make(map[string]struct{})
	dirs := make([]string, 0)
	for _, group := range groups {
		for _, dir := range group.SourceDirs() {
			if _, ok := seen[dir]; ok {
				continue
			}
			seen[dir] = struct{}{}
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return strings.Join(dirs, "|")
}

// pageSize returns the number of item rows visible in the job list area.
// It reserves lines for the title, cost table, help bar, and padding.
func (m launchModel) pageSize() int {
	if m.height <= 0 {
		return 20
	}
	// Reserve: title(2) + cost table(~12) + help(2) + padding(2) = ~18 lines
	return max(5, m.height-18)
}

// adjustOffset ensures the cursor is visible within the scrollable viewport.
func (m *launchModel) adjustOffset() {
	pageSize := m.pageSize()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+pageSize {
		m.offset = m.cursor - pageSize + 1
	}
	maxOffset := max(0, len(m.items)-pageSize)
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
}

func newLaunchModel(database *sql.DB, clients []cloud.Client, providerErr error, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts, predCfg *predictor.Config, gpuFilter string, projectFilter string, reconciling bool, fromWatch bool, inlineWatchEnabled bool) launchModel {
	items, selected, cursor := buildItemsFromGroups(groups)

	s := spinner.New()
	s.Spinner = spinner.Dot

	return launchModel{
		groups:                  groups,
		items:                   items,
		cursor:                  cursor,
		selected:                selected,
		reconciling:             reconciling,
		loading:                 true,
		database:                database,
		clients:                 clients,
		providerErr:             providerErr,
		appConfig:               cfg,
		launchOpts:              opts,
		predConfig:              predCfg,
		overheadModel:           buildOverheadModel(database),
		survivalModel:           buildSurvivalModel(database),
		progressCh:              make(chan estimateProgressMsg, 1),
		campaignCh:              make(chan int64, 1),
		planCh:                  make(chan launchExecutionPlanMsg, 1),
		phaseCh:                 make(chan launchPhaseMsg, 16),
		instanceCh:              make(chan launchInstanceRegisteredMsg, len(groups)),
		assetStageCh:            make(chan assetStageChangedMsg, 64),
		gpuFilter:               gpuFilter,
		projectFilter:           projectFilter,
		estimateCache:           make(map[string][]campaign.CostEstimate),
		reconciler:              campaign.NewReconciler(),
		spinner:                 s,
		fromWatch:               fromWatch,
		inlineWatchEnabled:      inlineWatchEnabled,
		registeredInstanceIDSet: make(map[int64]struct{}),
	}
}

func (m launchModel) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.spinner.Tick,
		m.fetchRawOffers(false),
		m.prefetchHFSizes(),
		m.startAssetStagingForGroups(m.groups),
	}
	if m.reconciling {
		cmds = append(cmds, m.runReconciliation())
	}
	return tea.Batch(cmds...)
}

// runReconciliation runs cloud instance reconciliation in the background and
// returns a refreshed job/group list.
func (m launchModel) runReconciliation() tea.Cmd {
	database := m.database
	cfg := m.appConfig
	gpuFilter := m.gpuFilter
	projectFilter := m.projectFilter
	reconciler := m.reconciler
	return func() tea.Msg {
		r2Client, _ := buildR2Client(cfg)
		var warn string
		if _, completed := syncCloudStateWithTimeout(cfg, database, reconciler, FastCloudSyncTimeout, false); !completed {
			warn = degraded.CloudSyncTimedOutShowingLastKnownState(FastCloudSyncTimeout.String()) + "."
		}

		// Re-query unplaced jobs since reconciliation may have freed some
		jobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return reconcileDoneMsg{err: fmt.Errorf("refresh unplaced jobs after reconciliation: %w", err)}
		}
		jobs = filterRentalLaunchJobs(jobs)
		jobs = filterLaunchJobsForScope(jobs, projectFilter)

		groups := campaign.PrepareGroups(jobs, database, gpuFilter, r2Client)

		return reconcileDoneMsg{groups: groups, warn: warn}
	}
}

func (m launchModel) fetchRawOffers(background bool) tea.Cmd {
	groups := m.groups
	clients := m.clients
	providerErr := m.providerErr
	database := m.database
	return func() tea.Msg {
		reusable, _ := campaign.FindReusableInstances(database)
		if len(clients) == 0 && len(reusable) == 0 {
			err := providerErr
			if err == nil {
				err = fmt.Errorf("no rental providers available")
			}
			return rawOffersLoadedMsg{err: err}
		}
		splitRaw := make([]campaign.GroupRawOffers, len(groups))
		for i, g := range groups {
			splitRaw[i] = campaign.GroupRawOffers{Group: g}
		}
		if len(clients) > 0 {
			splitRaw = campaign.FetchGroupRawOffers(clients, groups)
		}
		return rawOffersLoadedMsg{raw: splitRaw, reusable: reusable, background: background}
	}
}

func (m launchModel) buildProfilePlans(background bool) tea.Cmd {
	groups := m.groups
	clients := m.clients
	reusable := append([]campaign.InstanceCapacity(nil), m.reusable...)
	splitRaw := append([]campaign.GroupRawOffers(nil), m.cachedRawOffers...)
	database := m.database
	predCfg := m.predConfig
	overheadModel := m.overheadModel
	survivalModel := m.survivalModel
	minSurvival := m.launchOpts.MinSurvival
	return func() tea.Msg {
		profiles := bidding.ParetoSamplingProfiles()
		plans := campaign.BuildProfilePlansFromSplitRaw(
			database,
			clients,
			groups,
			splitRaw,
			reusable,
			predCfg,
			overheadModel,
			survivalModel,
			profiles,
			minSurvival,
		)
		options := campaign.BuildParetoTradeoffOptions(plans)
		return profilePlansLoadedMsg{plans: plans, options: options, background: background}
	}
}

// prefetchHFSizes warms the HF model size cache in parallel with offer fetching.
func (m launchModel) prefetchHFSizes() tea.Cmd {
	groups := m.groups
	return func() tea.Msg {
		inputs := campaign.CollectGroupInputs(groups)
		dataloc.PrefetchInputSizes(inputs, nil)
		return hfPrefetchDoneMsg{}
	}
}

func (m launchModel) planForTradeoff(id string) (campaign.StrategyPlan, bool) {
	if m.tradeoffPlans == nil || id == "" {
		return campaign.StrategyPlan{}, false
	}
	plan, ok := m.tradeoffPlans[id]
	return plan, ok
}

func (m launchModel) activeTradeoffPlan() (campaign.StrategyPlan, bool) {
	return m.planForTradeoff(m.activeTradeoff)
}

// rankCachedOffersForTradeoff returns display offers for a specific tradeoff option.
func (m launchModel) rankCachedOffersForTradeoff(id string) []campaign.GroupOffer {
	if plan, ok := m.planForTradeoff(id); ok {
		return plan.DisplayOffers
	}
	return nil
}

// rankCachedOffers ranks cached raw offers with the active tradeoff and
// caches the winning candidate result for launch.
func (m *launchModel) rankCachedOffers() []campaign.GroupOffer {
	plan, ok := m.activeTradeoffPlan()
	if !ok {
		m.winningCandidate = nil
		m.reuseAssignments = nil
		return nil
	}
	m.reuseAssignments = plan.ReuseAssignments
	if plan.NewCandidate != nil {
		result := *plan.NewCandidate
		m.winningCandidate = &result
	} else {
		m.winningCandidate = nil
	}
	return plan.DisplayOffers
}

// activeTradeoffEstimates returns the cost estimates for the active tradeoff.
// When a non-split candidate wins (e.g. "parallel"), returns approximate
// estimates built from the candidate's actual group offers, so the detail
// rows reflect the winning grouping (per-instance breakdown).
func (m launchModel) activeTradeoffEstimates() []campaign.CostEstimate {
	plan, ok := m.activeTradeoffPlan()
	if !ok {
		return nil
	}
	if plan.HasComplexExecution() {
		return plan.ActualEstimates
	}
	return plan.DisplayEstimates
}

func (m launchModel) activeTradeoffPredictionHint(selected []int) string {
	estimates := m.activeTradeoffEstimates()
	if len(estimates) == 0 {
		return ""
	}
	return campaign.SummarizeRuntimePredictions(estimates, selected).Hint()
}

func (m launchModel) visibleTradeoffs() []campaign.TradeoffOption {
	return m.tradeoffOptions
}

func tradeoffDisplayLabel(option campaign.TradeoffOption, index int) string {
	if option.Label != "" {
		return option.Label
	}
	return fmt.Sprintf("tradeoff %d", index+1)
}

func strategyMatchesTradeoffLabel(strategy bidding.SelectionStrategy, label string, optionCount int) bool {
	if label == "" {
		return false
	}
	if label == "cheap/fast/fastest" {
		return true
	}
	switch strategy {
	case bidding.StrategyCheap:
		return label == "cheap"
	case bidding.StrategyFast:
		return label == "fast"
	case bidding.StrategyFastest:
		return label == "fastest" || (optionCount == 2 && label == "fast")
	default:
		return false
	}
}

func (m *launchModel) adoptTradeoffOptions(options []campaign.TradeoffOption) {
	m.tradeoffOptions = append([]campaign.TradeoffOption(nil), options...)
	if len(m.tradeoffOptions) == 0 {
		m.activeTradeoff = ""
		m.tradeoffCursor = 0
		return
	}
	if m.activeTradeoff != "" {
		for i, option := range m.tradeoffOptions {
			if option.ID == m.activeTradeoff {
				m.tradeoffCursor = i
				return
			}
		}
	}
	for i, option := range m.tradeoffOptions {
		if strategyMatchesTradeoffLabel(m.launchOpts.Strategy, option.Label, len(m.tradeoffOptions)) {
			m.activeTradeoff = option.ID
			m.tradeoffCursor = i
			return
		}
	}
	if m.launchOpts.Strategy == bidding.StrategyFast && len(m.tradeoffOptions) > 0 {
		m.tradeoffCursor = len(m.tradeoffOptions) / 2
		m.activeTradeoff = m.tradeoffOptions[m.tradeoffCursor].ID
		return
	}
	m.activeTradeoff = m.tradeoffOptions[0].ID
	m.tradeoffCursor = 0
}

// activateTradeoffIfFocused sets the active tradeoff to the one under the
// tradeoff cursor and re-ranks offers. No-op if focus is on jobs.
func (m *launchModel) activateTradeoffIfFocused() {
	if m.focusArea != focusTradeoffs {
		return
	}
	tradeoffs := m.visibleTradeoffs()
	if m.tradeoffCursor >= len(tradeoffs) {
		m.tradeoffCursor = len(tradeoffs) - 1
	}
	if m.tradeoffCursor < 0 {
		return
	}
	newTradeoff := tradeoffs[m.tradeoffCursor]
	if newTradeoff.ID == m.activeTradeoff {
		return
	}
	m.activeTradeoff = newTradeoff.ID
	m.tradeoffRowsDirty = true
	if m.cachedRawOffers != nil {
		m.groupOffers = m.rankCachedOffers()
		m.costEstimates = m.activeTradeoffEstimates()
		if m.costEstimates == nil {
			m.costEstimates = m.estimateCache[offerIdentity(m.groupOffers)]
		}
	}
}

// fetchEstimatesIfNeeded returns a tea.Cmd that fetches cost estimates if the
// active tradeoff's estimates aren't cached yet. Returns nil if estimates are
// already available or offers haven't loaded.
func (m launchModel) fetchEstimatesIfNeeded() tea.Cmd {
	if m.costEstimates != nil || m.groupOffers == nil {
		return nil
	}
	key := offerIdentity(m.groupOffers)
	if _, ok := m.estimateCache[key]; ok {
		return nil
	}
	return tea.Batch(m.fetchEstimates(), waitForProgress(m.progressCh))
}

// gatherTradeoffRows builds a StrategySummaryRow for each visible tradeoff
// option using cached estimates. The time column uses total job completion
// time so the frontier labels and the displayed comparison metric match.
func (m launchModel) gatherTradeoffRows(selectedPerGroup []int) []campaign.StrategySummaryRow {
	var rows []campaign.StrategySummaryRow
	for i, option := range m.tradeoffOptions {
		isActive := option.ID == m.activeTradeoff
		plan, havePlan := m.planForTradeoff(option.ID)
		label := tradeoffDisplayLabel(option, i)

		row := campaign.StrategySummaryRow{
			Label:     label,
			Active:    isActive,
			Disclosed: isActive && m.tradeoffDisclosed,
			Loading:   true,
		}

		// Try detailed estimates first when the plan aligns with split groups.
		if havePlan && !plan.HasComplexExecution() {
			if summary := campaign.SummarizeTradeoffComparison(plan.DisplayEstimates, selectedPerGroup); summary != nil {
				row = *summary
				row.Label = label
				row.Active = isActive
				row.Disclosed = isActive && m.tradeoffDisclosed
			}
		} else if havePlan {
			key := offerIdentity(plan.DisplayOffers)
			if cached, ok := m.estimateCache[key]; ok {
				if summary := campaign.SummarizeTradeoffComparison(cached, selectedPerGroup); summary != nil {
					row = *summary
					row.Label = label
					row.Active = isActive
					row.Disclosed = isActive && m.tradeoffDisclosed
				}
			}
		}

		if havePlan && plan.HasComplexExecution() {
			if summary := campaign.SummarizeTradeoffExecutionEstimates(plan.ActualEstimates); summary != nil {
				summary.Label = row.Label
				summary.Active = row.Active
				summary.Disclosed = row.Disclosed
				row = *summary
			}
		}

		rows = append(rows, row)
	}
	return rows
}

// refreshTradeoffRowsIfNeeded recomputes cached tradeoff display data
// when the underlying data has changed. Call from Update(), not View().
func (m *launchModel) refreshTradeoffRowsIfNeeded() {
	if !m.tradeoffRowsDirty && m.cachedTradeoffRows != nil {
		return
	}
	selected := m.selectedCountByGroup()
	m.cachedTradeoffRows = m.gatherTradeoffRows(selected)
	m.cachedActiveEstimates = m.activeTradeoffEstimates()
	m.tradeoffRowsDirty = false
}

func (m launchModel) fetchEstimatesForOffers(offers []campaign.GroupOffer, cacheKey string, reportProgress bool) tea.Cmd {
	predConfig := m.predConfig
	ch := m.progressCh
	overheadModel := m.overheadModel
	survivalModel := m.survivalModel
	return func() tea.Msg {
		var onProgress func(string, int, int)
		if reportProgress {
			onProgress = func(phase string, resolved, total int) {
				select {
				case ch <- estimateProgressMsg{phase: phase, resolved: resolved, total: total}:
				default:
				}
			}
		}
		estimates := campaign.EstimateCosts(m.database, offers, predConfig, overheadModel, nil, survivalModel, onProgress)
		return estimatesLoadedMsg{estimates: estimates, cacheKey: cacheKey}
	}
}

func (m launchModel) fetchEstimates() tea.Cmd {
	return m.fetchEstimatesForOffers(m.groupOffers, offerIdentity(m.groupOffers), true)
}

// waitForCampaignCreated returns a Cmd that reads the campaign ID from the channel.
func (m launchModel) waitForCampaignCreated() tea.Cmd {
	ch := m.campaignCh
	return func() tea.Msg {
		id, ok := <-ch
		if !ok {
			return nil
		}
		return campaignCreatedMsg{campaignID: id}
	}
}

// waitForProgress returns a Cmd that reads one progress message from the channel.
func waitForProgress(ch chan estimateProgressMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// waitForLaunchPhase returns a Cmd that reads one launch phase update.
func waitForLaunchPhase(ch chan launchPhaseMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func waitForLaunchPlanReady(ch chan launchExecutionPlanMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func waitForLaunchInstanceRegistered(ch chan launchInstanceRegisteredMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func waitForAssetStage(ch chan assetStageChangedMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m launchModel) waitForLaunchHeartbeat() tea.Cmd {
	if !m.launching {
		return nil
	}
	database := m.database
	campaignID := m.campaignID
	return tea.Tick(launchHeartbeatInterval, func(t time.Time) tea.Msg {
		out := launchHeartbeatMsg{polledAt: t}
		if database == nil || campaignID == 0 {
			return out
		}
		instances, err := db.GetCampaignInstances(database, campaignID)
		if err != nil {
			out.err = err
			return out
		}
		if len(instances) == 0 {
			return out
		}
		out.instanceIDs = make([]int64, 0, len(instances))
		out.statusCounts = make(map[string]int)
		for _, inst := range instances {
			out.instanceIDs = append(out.instanceIDs, inst.ID)
			out.statusCounts[inst.Status]++
		}
		sort.Slice(out.instanceIDs, func(i, j int) bool { return out.instanceIDs[i] < out.instanceIDs[j] })
		return out
	})
}

func hasLaunchR2Config(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	r2Cfg := cfg.Vastai.R2
	return strings.TrimSpace(r2Cfg.AccountID) != "" &&
		strings.TrimSpace(r2Cfg.AccessKeyID) != "" &&
		strings.TrimSpace(r2Cfg.SecretAccessKey) != "" &&
		strings.TrimSpace(r2Cfg.Bucket) != ""
}

func (m launchModel) startAssetStagingForGroups(groups []campaign.InstanceGroup) tea.Cmd {
	if !hasLaunchR2Config(m.appConfig) || len(groups) == 0 {
		return nil
	}
	signature := launchAssetStageSignature(groups)
	if signature == "" || signature == m.assetStageSignature {
		return nil
	}
	phaseCh := m.assetStageCh
	r2Cfg := m.appConfig.Vastai.R2.ToCloudR2Config()
	return func() tea.Msg {
		stager, err := campaign.StartR2AssetStagingWithReporter(r2Cfg, groups, func(_ campaign.AssetStageStatus) {
			select {
			case phaseCh <- assetStageChangedMsg{signature: signature}:
			default:
			}
		})
		return assetStageStartedMsg{signature: signature, stager: stager, err: err}
	}
}

func (m launchModel) updateInlineWatch(msg tea.Msg) (launchModel, tea.Cmd, bool) {
	if m.inlineWatch == nil {
		return m, nil, false
	}
	next, cmd := m.inlineWatch.Update(msg)
	watch, ok := next.(watchModel)
	if ok {
		m.inlineWatch = &watch
	}
	return m, cmd, true
}

func (m launchModel) maybeStartInlineWatch() (launchModel, tea.Cmd) {
	if !m.inlineWatchEnabled || m.inlineWatch != nil {
		return m, nil
	}
	// Start inline watch only after at least one instance is known, so launch
	// feedback appears immediately and we avoid showing unrelated inventory rows.
	if len(m.registeredInstanceIDs) == 0 {
		return m, nil
	}
	var r2Client *r2.Client
	if m.appConfig != nil {
		r2Client, _ = buildR2Client(m.appConfig)
	}
	inlineWatch := newInstanceWatchModel(m.database, append([]int64(nil), m.registeredInstanceIDs...), r2Client, m.appConfig)
	inlineWatch.launchPending = m.launching
	inlineWatch.campaignID = m.campaignID
	if inlineWatch.launchedAt.IsZero() && m.campaignID != 0 {
		inlineWatch.launchedAt = time.Now()
	}
	if summary := campaign.SummarizeEstimates(m.costEstimates); summary != nil {
		inlineWatch.estimateSummaryLine = summary.FormatLine()
	}
	m.inlineWatch = &inlineWatch
	m.inlineWatchUsed = true
	return m, inlineWatch.Init()
}

// quitOrSwitchToWatch returns tea.Quit for standalone launch, or
// switchToWatchMsg when embedded in the watch router.
func (m launchModel) quitOrSwitchToWatch(flash string) tea.Cmd {
	if m.fromWatch {
		return func() tea.Msg { return switchToWatchMsg{flash: flash} }
	}
	return tea.Quit
}

func (m launchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if sizeMsg, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = sizeMsg.Width
		m.height = sizeMsg.Height
		m.adjustOffset()
		if m.inlineWatch != nil {
			next, cmd, _ := m.updateInlineWatch(msg)
			return next, cmd
		}
		return m, nil
	}

	if m.inlineWatch != nil {
		switch msg.(type) {
		case campaignCreatedMsg, launchPhaseMsg, launchInstanceRegisteredMsg, instancesLaunchedMsg, switchToLaunchMsg, assetStageStartedMsg, assetStageChangedMsg, launchHeartbeatMsg:
		default:
			next, cmd, handled := m.updateInlineWatch(msg)
			if handled {
				return next, cmd
			}
		}
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.inlineWatch != nil {
			next, cmd, _ := m.updateInlineWatch(msg)
			return next, cmd
		}
		result, cmd := m.handleKey(msg)
		if lm, ok := result.(launchModel); ok {
			lm.refreshTradeoffRowsIfNeeded()
			return lm, cmd
		}
		return result, cmd

	case hfPrefetchDoneMsg:
		return m, nil // cache is warmed; EstimateCosts will benefit

	case switchToLaunchMsg:
		// Return from inline watch to launch selection with refreshed jobs
		m.inlineWatch = nil
		m.inlineWatchUsed = false
		m.reconciling = true
		return m, m.runReconciliation()

	case reconcileDoneMsg:
		m.reconciling = false
		if msg.err != nil {
			m.statusHint = fmt.Sprintf("Reconciliation failed: %v", msg.err)
			return m, nil
		}
		if msg.warn != "" {
			m.statusHint = msg.warn
		}
		if msg.groups == nil {
			m.statusHint = "Reconciliation returned no data; keeping current state."
			return m, nil
		}
		if len(msg.groups) == 0 {
			// All jobs got placed during reconciliation
			m.err = fmt.Errorf("no jobs need rental GPUs (all placed during reconciliation)")
			return m, m.quitOrSwitchToWatch("All jobs placed during reconciliation.")
		}
		// Only re-fetch offers if groups actually changed
		if groupsChanged(m.groups, msg.groups) {
			oldCount := countGroupJobs(m.groups)
			newCount := countGroupJobs(msg.groups)
			if newCount < oldCount {
				m.reconcileDropped = oldCount - newCount
			} else {
				m.reconcileDropped = 0
			}
			m.groups = msg.groups
			m.items, m.selected, m.cursor = buildItemsFromGroups(m.groups)
			m.loading = true
			m.groupOffers = nil
			m.cachedRawOffers = nil
			m.reusable = nil
			m.tradeoffPlans = nil
			m.tradeoffOptions = nil
			m.activeTradeoff = ""
			m.reuseAssignments = nil
			m.winningCandidate = nil
			m.costEstimates = nil
			m.estimateCache = make(map[string][]campaign.CostEstimate)
			m.tradeoffRowsDirty = true
			m.cachedTradeoffRows = nil
			if m.assetStager != nil {
				m.assetStager.Close()
			}
			m.assetStager = nil
			m.assetStageSignature = ""
			m.assetStageErr = nil
			return m, tea.Batch(
				m.fetchRawOffers(false),
				m.startAssetStagingForGroups(msg.groups),
			)
		}
		return m, nil

	case rawOffersLoadedMsg:
		if msg.err != nil {
			m.loading = false
			m.err = msg.err
			return m, m.quitOrSwitchToWatch(fmt.Sprintf("Launch error: %v", msg.err))
		}
		m.cachedRawOffers = msg.raw
		m.reusable = append([]campaign.InstanceCapacity(nil), msg.reusable...)
		m.tradeoffRowsDirty = true
		if !msg.background {
			m.loading = true
		}
		m.refreshTradeoffRowsIfNeeded()
		return m, m.buildProfilePlans(msg.background)

	case profilePlansLoadedMsg:
		if msg.err != nil {
			m.loading = false
			m.err = msg.err
			return m, m.quitOrSwitchToWatch(fmt.Sprintf("Launch error: %v", msg.err))
		}
		m.tradeoffPlans = msg.plans
		m.adoptTradeoffOptions(msg.options)
		for _, plan := range msg.plans {
			if len(plan.DisplayOffers) == 0 {
				continue
			}
			m.estimateCache[offerIdentity(plan.DisplayOffers)] = plan.DisplayEstimates
		}
		m.tradeoffRowsDirty = true
		oldOffers := m.groupOffers
		m.groupOffers = m.rankCachedOffers()
		if !msg.background {
			m.loading = false
		}
		key := offerIdentity(m.groupOffers)
		if offersMatch(oldOffers, m.groupOffers) && m.costEstimates != nil {
			m.refreshTradeoffRowsIfNeeded()
			return m, nil
		}
		m.costEstimates = m.activeTradeoffEstimates()
		if m.costEstimates == nil {
			m.costEstimates = m.estimateCache[key]
		}
		m.refreshTradeoffRowsIfNeeded()
		return m, nil

	case estimateProgressMsg:
		m.estimateProgress = msg
		if m.costEstimates == nil {
			return m, waitForProgress(m.progressCh)
		}
		return m, nil

	case estimatesLoadedMsg:
		m.estimateCache[msg.cacheKey] = msg.estimates
		m.tradeoffRowsDirty = true
		// Only update display if this is for the current tradeoff's offers
		if msg.cacheKey == offerIdentity(m.groupOffers) {
			m.costEstimates = msg.estimates
		}
		m.refreshTradeoffRowsIfNeeded()
		return m, nil

	case campaignCreatedMsg:
		m.campaignID = msg.campaignID
		m.lastLaunchProgressAt = time.Now()
		if m.inlineWatch != nil {
			m.inlineWatch.campaignID = msg.campaignID
			if m.inlineWatch.launchedAt.IsZero() {
				m.inlineWatch.launchedAt = time.Now()
			}
		}
		return m, nil

	case launchPhaseMsg:
		m.lastLaunchProgressAt = time.Now()
		if msg.groupIndex < 0 {
			m.campaignPhase = msg.phase
		} else {
			if m.groupPhases == nil {
				m.groupPhases = make(map[int]string)
			}
			m.groupPhases[msg.groupIndex] = msg.phase
		}
		if m.launching {
			return m, waitForLaunchPhase(m.phaseCh)
		}
		return m, nil

	case assetStageStartedMsg:
		if msg.signature != launchAssetStageSignature(m.groups) {
			if msg.stager != nil {
				msg.stager.Close()
			}
			return m, nil
		}
		if m.assetStager != nil && m.assetStager != msg.stager {
			m.assetStager.Close()
		}
		m.assetStager = msg.stager
		m.assetStageSignature = msg.signature
		m.assetStageErr = msg.err
		if msg.err != nil {
			return m, nil
		}
		return m, waitForAssetStage(m.assetStageCh)

	case assetStageChangedMsg:
		if msg.signature != m.assetStageSignature {
			return m, waitForAssetStage(m.assetStageCh)
		}
		return m, waitForAssetStage(m.assetStageCh)

	case launchExecutionPlanMsg:
		m.expectedInstanceCount = msg.expectedInstanceCount
		return m, nil

	case launchInstanceRegisteredMsg:
		m.lastLaunchProgressAt = time.Now()
		if _, ok := m.registeredInstanceIDSet[msg.instanceID]; !ok {
			m.registeredInstanceIDSet[msg.instanceID] = struct{}{}
			m.registeredInstanceIDs = append(m.registeredInstanceIDs, msg.instanceID)
		}
		if m.groupDone == nil {
			m.groupDone = make(map[int]bool)
		}
		m.groupDone[msg.groupIndex] = true
		cmds := []tea.Cmd{}
		if m.launching {
			cmds = append(cmds, waitForLaunchInstanceRegistered(m.instanceCh))
		}
		if m.inlineWatch != nil {
			if cmd := m.inlineWatch.addInstance(msg.instanceID); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		next, watchCmd := m.maybeStartInlineWatch()
		m = next
		if watchCmd != nil {
			cmds = append(cmds, watchCmd)
		}
		if len(cmds) == 0 {
			return m, nil
		}
		return m, tea.Batch(cmds...)

	case launchHeartbeatMsg:
		cmds := make([]tea.Cmd, 0, 2)
		if msg.err != nil {
			m.launchHeartbeatErr = msg.err
		} else {
			m.launchHeartbeatErr = nil
			if len(msg.statusCounts) > 0 {
				m.launchStatusCounts = msg.statusCounts
			}
			for _, id := range msg.instanceIDs {
				if _, ok := m.registeredInstanceIDSet[id]; ok {
					continue
				}
				m.registeredInstanceIDSet[id] = struct{}{}
				m.registeredInstanceIDs = append(m.registeredInstanceIDs, id)
			}
			next, watchCmd := m.maybeStartInlineWatch()
			m = next
			if watchCmd != nil {
				cmds = append(cmds, watchCmd)
			}
		}
		if m.launching {
			cmds = append(cmds, m.waitForLaunchHeartbeat())
		}
		if len(cmds) == 0 {
			return m, nil
		}
		return m, tea.Batch(cmds...)

	case instancesLaunchedMsg:
		m.launching = false
		inlineWatchCmds := make([]tea.Cmd, 0)
		if m.inlineWatch != nil {
			m.inlineWatch.launchPending = false
			for _, id := range msg.instanceIDs {
				if cmd := m.inlineWatch.addInstance(id); cmd != nil {
					inlineWatchCmds = append(inlineWatchCmds, cmd)
				}
			}
		}
		if msg.err != nil {
			m.err = msg.err
			if m.inlineWatch != nil {
				if len(inlineWatchCmds) > 0 {
					return m, tea.Batch(inlineWatchCmds...)
				}
				return m, nil
			}
			return m, m.quitOrSwitchToWatch(fmt.Sprintf("Launch error: %v", msg.err))
		}
		m.done = true
		m.instanceIDs = msg.instanceIDs
		m.partialErrors = make([]string, 0, len(msg.errors))
		for _, err := range msg.errors {
			if err != nil {
				m.partialErrors = append(m.partialErrors, err.Error())
			}
		}
		if m.inlineWatch != nil {
			// Pass partial errors and unplaced jobs to the watch model for retry
			if len(m.partialErrors) > 0 {
				m.inlineWatch.partialErrors = m.partialErrors
				if m.database != nil {
					unplaced, _ := db.ListUnplacedJobs(m.database)
					m.inlineWatch.partialErrorJobs = unplaced
				}
			}
			if len(inlineWatchCmds) > 0 {
				return m, tea.Batch(inlineWatchCmds...)
			}
			return m, nil
		}
		if len(m.partialErrors) == 0 {
			if m.campaignID == 0 && m.expectedInstanceCount == 0 && len(m.instanceIDs) > 0 {
				return m, m.quitOrSwitchToWatch("Submitted jobs to existing instances.")
			}
			return m, m.quitOrSwitchToWatch(formatLaunchResultFlash(m.instanceIDs, m.partialErrors))
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m launchModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.launching && m.inlineWatch == nil {
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, m.quitOrSwitchToWatch("Launch continues in background.")
		case "ctrl+z":
			return m, tea.Suspend
		default:
			return m, nil // ignore other keys while launching
		}
	}

	switch msg.String() {
	case "enter":
		if m.done {
			return m, m.quitOrSwitchToWatch(formatLaunchResultFlash(m.instanceIDs, m.partialErrors))
		}
		if m.loading || m.reconciling {
			status := "Loading launch data"
			switch {
			case m.reconciling:
				status = "Reconciling"
			case m.fetchingOffers():
				status = "Searching for offers"
			case m.buildingPlans():
				status = "Building launch plan"
			}
			m.statusHint = status + "… press Enter again when ready"
			return m, nil
		}
		m.statusHint = ""
		// Count selected
		count := 0
		for _, v := range m.selected {
			if v {
				count++
			}
		}
		if count == 0 {
			return m, m.quitOrSwitchToWatch("Launch canceled.")
		}
		m.launching = true
		m.expectedInstanceCount = 0
		m.launchStartedAt = time.Now()
		m.lastLaunchProgressAt = m.launchStartedAt
		m.launchStatusCounts = nil
		m.launchHeartbeatErr = nil
		m.registeredInstanceIDs = nil
		m.registeredInstanceIDSet = make(map[int64]struct{})
		m.inlineWatch = nil
		m.inlineWatchUsed = false
		m.partialErrors = nil
		m.err = nil
		m.reconcileDropped = 0
		m.instanceIDs = nil
		m.campaignPhase = ""
		m.groupPhases = make(map[int]string)
		m.groupDone = make(map[int]bool)
		cmds := []tea.Cmd{
			m.spinner.Tick,
			m.waitForLaunchHeartbeat(),
			m.launchInstances(),
			m.waitForCampaignCreated(),
			waitForLaunchPlanReady(m.planCh),
			waitForLaunchPhase(m.phaseCh),
			waitForLaunchInstanceRegistered(m.instanceCh),
		}
		return m, tea.Batch(cmds...)

	case "q", "esc", "ctrl+c":
		return m, m.quitOrSwitchToWatch("Launch canceled.")

	case "up", "k":
		if m.focusArea == focusJobs {
			if m.cursor > 0 {
				m.cursor--
			} else {
				// Wrap to bottom of tradeoffs.
				tradeoffs := m.visibleTradeoffs()
				if len(tradeoffs) > 0 {
					m.focusArea = focusTradeoffs
					m.tradeoffCursor = len(tradeoffs) - 1
					m.activateTradeoffIfFocused()
				}
			}
		} else {
			if m.tradeoffCursor > 0 {
				m.tradeoffCursor--
				m.activateTradeoffIfFocused()
			} else {
				// Move to bottom of jobs
				m.focusArea = focusJobs
				m.cursor = max(0, len(m.items)-1)
			}
		}
		m.adjustOffset()
		return m, m.fetchEstimatesIfNeeded()

	case "down":
		if m.focusArea == focusJobs {
			if m.cursor < len(m.items)-1 {
				m.cursor++
			} else {
				// Move to top of tradeoffs.
				tradeoffs := m.visibleTradeoffs()
				if len(tradeoffs) > 0 {
					m.focusArea = focusTradeoffs
					m.tradeoffCursor = 0
					m.activateTradeoffIfFocused()
				}
			}
		} else {
			tradeoffs := m.visibleTradeoffs()
			if m.tradeoffCursor < len(tradeoffs)-1 {
				m.tradeoffCursor++
				m.activateTradeoffIfFocused()
			} else {
				// Wrap to top of jobs
				m.focusArea = focusJobs
				m.cursor = 0
			}
		}
		m.adjustOffset()
		return m, m.fetchEstimatesIfNeeded()

	case "j":
		// Jump between jobs and tradeoffs.
		if m.focusArea == focusJobs {
			tradeoffs := m.visibleTradeoffs()
			if len(tradeoffs) > 0 {
				m.focusArea = focusTradeoffs
				m.activateTradeoffIfFocused()
			}
		} else {
			m.focusArea = focusJobs
		}
		return m, m.fetchEstimatesIfNeeded()

	case "pgup":
		m.cursor -= m.pageSize()
		if m.cursor < 0 {
			m.cursor = 0
		}
		m.focusArea = focusJobs
		m.adjustOffset()
		return m, nil

	case "pgdown":
		m.cursor += m.pageSize()
		if m.cursor >= len(m.items) {
			m.cursor = max(0, len(m.items)-1)
		}
		m.focusArea = focusJobs
		m.adjustOffset()
		return m, nil

	case " ":
		if m.focusArea == focusJobs && m.cursor < len(m.items) {
			item := m.items[m.cursor]
			if item.isHeader {
				m.toggleGroup(item.groupIdx)
			} else {
				id := item.jobID
				m.selected[id] = !m.selected[id]
			}
			m.tradeoffRowsDirty = true
		}
		// Space in tradeoff area is a no-op (active option already selected).
		return m, nil

	case "d":
		m.showCostDetail = !m.showCostDetail
		return m, nil

	case "right":
		tradeoffs := m.visibleTradeoffs()
		if len(tradeoffs) == 0 {
			return m, nil
		}
		// Disclose from anywhere, anchoring on the current active strategy.
		if m.focusArea != focusTradeoffs {
			m.focusArea = focusTradeoffs
			m.tradeoffCursor = 0
			for i, option := range tradeoffs {
				if option.ID == m.activeTradeoff {
					m.tradeoffCursor = i
					break
				}
			}
		}
		m.tradeoffDisclosed = true
		m.tradeoffRowsDirty = true
		return m, nil

	case "left":
		if m.focusArea == focusTradeoffs {
			m.tradeoffDisclosed = false
			m.tradeoffRowsDirty = true
		}
		return m, nil

	case "a":
		for id := range m.selected {
			m.selected[id] = true
		}
		m.tradeoffRowsDirty = true
		return m, nil

	case "n":
		for id := range m.selected {
			m.selected[id] = false
		}
		m.tradeoffRowsDirty = true
		return m, nil

	case "s":
		// Switch to tradeoff options area and cycle among them.
		tradeoffs := m.visibleTradeoffs()
		if len(tradeoffs) == 0 {
			return m, nil
		}
		if m.focusArea != focusTradeoffs {
			// Enter the tradeoff area at the current active option.
			m.focusArea = focusTradeoffs
			for i, option := range tradeoffs {
				if option.ID == m.activeTradeoff {
					m.tradeoffCursor = i
					break
				}
			}
		}
		// Cycle to next option (with wrap).
		m.tradeoffCursor = (m.tradeoffCursor + 1) % len(tradeoffs)
		m.activateTradeoffIfFocused()
		return m, m.fetchEstimatesIfNeeded()

	}

	return m, nil
}

// toggleGroup toggles all jobs in the given group. If any are unselected,
// select all; otherwise deselect all.
func (m launchModel) toggleGroup(groupIdx int) {
	// Check if all jobs in this group are selected
	allSelected := true
	for _, item := range m.items {
		if !item.isHeader && item.groupIdx == groupIdx {
			if !m.selected[item.jobID] {
				allSelected = false
				break
			}
		}
	}
	// Toggle: if all selected, deselect all; otherwise select all
	newState := !allSelected
	for _, item := range m.items {
		if !item.isHeader && item.groupIdx == groupIdx {
			m.selected[item.jobID] = newState
		}
	}
}

// groupCheckState returns "[x]" if all jobs selected, "[-]" if some, "[ ]" if none.
func (m launchModel) groupCheckState(groupIdx int) string {
	total, selected := 0, 0
	for _, item := range m.items {
		if !item.isHeader && item.groupIdx == groupIdx {
			total++
			if m.selected[item.jobID] {
				selected++
			}
		}
	}
	switch {
	case selected == total:
		return "[x]"
	case selected > 0:
		return "[-]"
	default:
		return "[ ]"
	}
}

// costEstimateHeader renders the section header for the cost estimate table.
func costEstimateHeader(rough bool) string {
	s := "── Cost Estimate"
	if rough {
		s += " (calculating)"
	}
	s += " ─────────────────────────────"
	return launchDimStyle.Render(s)
}

func renderRawOfferSummary(b *strings.Builder, raw []campaign.GroupRawOffers) {
	if len(raw) == 0 {
		return
	}
	type summaryRow struct {
		label  string
		status string
		groups int
	}

	statusFor := func(go_ campaign.GroupRawOffers) string {
		switch {
		case go_.Err != nil:
			return "search error"
		case len(go_.Offers) == 0:
			return "0 direct offers"
		case len(go_.Offers) == 1:
			return "1 direct offer"
		default:
			return fmt.Sprintf("%d direct offers", len(go_.Offers))
		}
	}

	matchedGroups := 0
	rows := make([]summaryRow, 0, len(raw))
	for _, go_ := range raw {
		if go_.Err == nil && len(go_.Offers) > 0 {
			matchedGroups++
		}
		row := summaryRow{
			label:  launchSummaryGPUSpec(go_.Group),
			status: statusFor(go_),
			groups: 1,
		}
		if n := len(rows); n > 0 && rows[n-1].label == row.label && rows[n-1].status == row.status {
			rows[n-1].groups++
			continue
		}
		rows = append(rows, row)
	}
	b.WriteString(launchDimStyle.Render(fmt.Sprintf(" Direct offers found for %d/%d GPU groups.", matchedGroups, len(raw))))
	b.WriteString("\n")

	limit := min(len(rows), 5)
	for i := 0; i < limit; i++ {
		row := rows[i]
		label := row.label
		if row.groups > 1 {
			label = fmt.Sprintf("%s (%d groups)", row.label, row.groups)
		}
		b.WriteString(launchDimStyle.Render(fmt.Sprintf("  %s: %s", label, row.status)))
		b.WriteString("\n")
	}
	if len(rows) > limit {
		b.WriteString(launchDimStyle.Render(fmt.Sprintf("  … %d more GPU groups", len(rows)-limit)))
		b.WriteString("\n")
	}
}

func launchSummaryGPUSpec(group campaign.InstanceGroup) string {
	if group.GPUClass == "" && group.GPUMemGB <= 0 && group.MaxGPUMemGB <= 0 {
		return "Unspecified GPU"
	}
	if group.GPUMemGB <= 0 || group.MaxGPUMemGB <= 0 {
		return group.GPUSpec()
	}

	prefix := strings.TrimSpace(group.GPUClass)
	if prefix != "" {
		prefix += " "
	}

	switch {
	case group.MaxGPUMemGB == group.GPUMemGB:
		return fmt.Sprintf("%s%d GB", prefix, group.GPUMemGB)
	case group.MaxGPUMemGB > group.GPUMemGB:
		return fmt.Sprintf("%s%d-%d GB", prefix, group.GPUMemGB, group.MaxGPUMemGB)
	default:
		return group.GPUSpec()
	}
}

// renderCostTable writes a CostTable to the builder, dimming lines as needed.
func renderCostTable(b *strings.Builder, table campaign.CostTable) {
	for _, cl := range table.Lines {
		if cl.Dimmed {
			b.WriteString(launchDimStyle.Render(cl.Text))
		} else {
			b.WriteString(cl.Text)
		}
		b.WriteString("\n")
	}
}

// selectedCountByGroup returns the number of selected jobs per group.
func (m launchModel) selectedCountByGroup() []int {
	counts := make([]int, len(m.groups))
	for _, item := range m.items {
		if !item.isHeader && m.selected[item.jobID] {
			counts[item.groupIdx]++
		}
	}
	return counts
}

// offersLoaded reports whether the async offer fetch has completed.
// When false, groupHasOffer returns false for all groups (offers not yet known).
func (m launchModel) offersLoaded() bool {
	return m.groupOffers != nil
}

func (m launchModel) rawOffersLoaded() bool {
	return m.cachedRawOffers != nil
}

func (m launchModel) fetchingOffers() bool {
	return m.loading && !m.rawOffersLoaded()
}

func (m launchModel) buildingPlans() bool {
	return m.loading && m.rawOffersLoaded() && m.tradeoffPlans == nil
}

// groupHasOffer reports whether the group at groupIdx has a matching offer.
// Returns false when offers haven't been fetched yet (nil slice).
func (m launchModel) groupHasOffer(groupIdx int) bool {
	return m.offersLoaded() && groupIdx < len(m.groupOffers) && m.groupOffers[groupIdx].Offer != nil
}

func (m launchModel) selectedLaunchGroupCount() int {
	count := 0
	for i, group := range m.groups {
		if !m.groupHasOffer(i) {
			continue
		}
		for _, job := range group.Jobs {
			if m.selected[job.ID] {
				count++
				break
			}
		}
	}
	return count
}

func (m launchModel) selectedNewLaunchGroupCount() int {
	if m.winningCandidate == nil {
		return 0
	}
	count := 0
	for i, group := range m.winningCandidate.Groups {
		if i >= len(m.winningCandidate.Offers) || m.winningCandidate.Offers[i].Offer == nil {
			continue
		}
		for _, job := range group.Jobs {
			if m.selected[job.ID] {
				count++
				break
			}
		}
	}
	return count
}

// selectedLaunchableJobCount returns the number of selected jobs in groups that have offers.
func (m launchModel) selectedLaunchableJobCount() int {
	count := 0
	for _, item := range m.items {
		if !item.isHeader && m.selected[item.jobID] && m.groupHasOffer(item.groupIdx) {
			count++
		}
	}
	return count
}

func (m launchModel) launchInstances() tea.Cmd {
	database := m.database
	clients := m.clients
	providerErr := m.providerErr
	cfg := m.appConfig
	opts := m.launchOpts
	profile := opts.ScoringProfile()
	if plan, ok := m.activeTradeoffPlan(); ok && plan.Profile.Valid() {
		profile = plan.Profile
		opts.ScoreProfile = plan.Profile
	}
	campaignCh := m.campaignCh
	planCh := m.planCh
	phaseCh := m.phaseCh
	instanceCh := m.instanceCh
	predCfg := m.predConfig
	overheadModel := m.overheadModel
	survivalModel := m.survivalModel
	assetStager := m.assetStager

	return func() tea.Msg {
		defer close(planCh)
		defer close(campaignCh)
		defer close(phaseCh)
		defer close(instanceCh)
		var launchGroups []campaign.InstanceGroup
		sendLaunchPlanReady := func(expectedInstanceCount int) {
			select {
			case planCh <- launchExecutionPlanMsg{expectedInstanceCount: expectedInstanceCount}:
			default:
			}
		}
		sendCampaignPhase := func(phase string) {
			select {
			case phaseCh <- launchPhaseMsg{groupIndex: -1, phase: phase}:
			default:
			}
		}
		sendGroupPhase := func(groupIdx int, phase string) {
			select {
			case phaseCh <- launchPhaseMsg{groupIndex: groupIdx, phase: phase}:
			default:
			}
		}
		sendInstanceRegistered := func(groupIdx int, instanceID int64) {
			select {
			case instanceCh <- launchInstanceRegisteredMsg{instanceID: instanceID, groupIndex: groupIdx}:
			default:
			}
		}
		findGroupIndex := func(group campaign.InstanceGroup) int {
			for i, lg := range launchGroups {
				if lg.GPUClass == group.GPUClass && lg.GPUMemGB == group.GPUMemGB {
					return i
				}
			}
			return -1
		}
		sendCampaignPhase("launching worker instances")

		prep, err := prepareLaunchExecutionPlan(
			database,
			clients,
			providerErr,
			m.groups,
			m.selected,
			profile,
			opts.MinSurvival,
			predCfg,
			overheadModel,
			survivalModel,
		)
		if err != nil {
			sendLaunchPlanReady(0)
			return instancesLaunchedMsg{err: err}
		}

		launchGroups = prep.LaunchGroups
		offers := prep.Offers
		launchGroupOffers := prep.LaunchGroupOffers
		selectedEstimates := prep.Estimates
		selectedReuse := prep.StrategyPlan.ReuseAssignments
		if (opts.MaxSpendCents == 0 || opts.MaxTimeSeconds == 0) && len(selectedEstimates) > 0 {
			opts.ApplyAutoBudget(selectedEstimates)
		}
		sendLaunchPlanReady(len(launchGroups))

		if len(selectedReuse) > 0 {
			r2Client, err := newR2ClientFromConfig()
			if err != nil {
				return instancesLaunchedMsg{err: fmt.Errorf("R2 client for reuse: %w", err)}
			}
			if err := executeReuseAssignments(database, r2Client, selectedReuse); err != nil {
				return instancesLaunchedMsg{err: err}
			}
		}
		if len(launchGroups) == 0 {
			if len(selectedReuse) == 0 {
				return instancesLaunchedMsg{err: fmt.Errorf("no selected groups have available offers")}
			}
			return instancesLaunchedMsg{instanceIDs: uniqueReuseInstanceIDs(selectedReuse)}
		}
		// If async estimation has not finished yet, compute estimates now so
		// launched campaigns still persist estimated_cost_cents.
		if len(selectedEstimates) == 0 {
			selectedEstimates = campaign.EstimateCosts(m.database, launchGroupOffers, predCfg, overheadModel, nil, survivalModel, nil)
		}

		r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
		sendCampaignPhase("preparing campaign launch")

		result, err := campaign.LaunchCampaignWithAssetStager(
			assetStager,
			clients, database, launchGroups, offers, selectedEstimates, survivalModel, opts, r2Cfg,
			func(provider cloud.Provider) (cloud.CreateOpts, error) {
				return createOptsForProvider(cfg, provider)
			},
			func(group campaign.InstanceGroup, phase string) {
				if strings.EqualFold(group.GPUClass, "campaign") {
					sendCampaignPhase(phase)
					return
				}
				if idx := findGroupIndex(group); idx >= 0 {
					sendGroupPhase(idx, phase)
				} else {
					sendCampaignPhase(fmt.Sprintf("%s: %s", group.GPUSpec(), phase))
				}
			},
			func(id int64) { campaignCh <- id },
			func(group campaign.InstanceGroup, instanceID int64) {
				sendInstanceRegistered(findGroupIndex(group), instanceID)
			},
		)
		if err != nil {
			return instancesLaunchedMsg{err: err}
		}

		if len(result.InstanceIDs) == 0 && len(result.Errors) > 0 {
			return instancesLaunchedMsg{err: result.Errors[0], errors: result.Errors}
		}

		return instancesLaunchedMsg{instanceIDs: result.InstanceIDs, errors: result.Errors}
	}
}

func uniqueReuseInstanceIDs(assignments []campaign.ReuseAssignment) []int64 {
	seen := make(map[int64]struct{})
	var ids []int64
	for _, assignment := range assignments {
		id := assignment.Instance.Instance.ID
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func countRenderedLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n")
}

func formatAssetStatusDetail(status campaign.AssetStageStatus, fallbackLabel string) string {
	label := strings.TrimSpace(status.Label)
	if label == "" {
		label = fallbackLabel
	}
	phase := status.Phase
	switch {
	case status.Err != nil:
		return fmt.Sprintf("%s: error (%v)", label, status.Err)
	case status.Ready:
		return fmt.Sprintf("%s: ready", label)
	case phase != "":
		return fmt.Sprintf("%s: %s", label, phase)
	default:
		return fmt.Sprintf("%s: pending", label)
	}
}

func (m launchModel) formatAssetProgressForDirs(dirs []string) string {
	if m.assetStager == nil {
		return ""
	}
	statuses := m.assetStager.SnapshotStatuses()
	if len(statuses) == 0 {
		return ""
	}

	total := 1
	ready := 0
	details := make([]string, 0, len(dirs)+1)

	agentStatus, ok := statuses["agent"]
	if ok {
		if agentStatus.Ready {
			ready++
		} else {
			details = append(details, formatAssetStatusDetail(agentStatus, "agent"))
		}
	} else {
		details = append(details, "agent: pending")
	}

	for _, dir := range dirs {
		total++
		status, ok := statuses[dir]
		label := filepath.Base(dir)
		if !ok {
			details = append(details, fmt.Sprintf("%s: pending", label))
			continue
		}
		if status.Ready {
			ready++
			continue
		}
		details = append(details, formatAssetStatusDetail(status, label))
	}

	percent := 0
	if total > 0 {
		percent = ready * 100 / total
	}
	if len(details) == 0 {
		return fmt.Sprintf("staging (%d/%d assets ready, %d%%)", ready, total, percent)
	}
	return fmt.Sprintf("staging (%d/%d assets ready, %d%%; %s)", ready, total, percent, strings.Join(details, "; "))
}

func (m launchModel) formatBackgroundAssetSummary() string {
	if m.assetStageErr != nil {
		return fmt.Sprintf("Pre-staging unavailable: %v", m.assetStageErr)
	}
	if m.assetStager == nil {
		return ""
	}
	ready, total := m.assetStager.AssetCounts()
	if total == 0 {
		return ""
	}
	statuses := m.assetStager.SnapshotStatuses()
	details := make([]string, 0, total)
	if status, ok := statuses["agent"]; ok && !status.Ready {
		details = append(details, formatAssetStatusDetail(status, "agent"))
	}
	keys := make([]string, 0, len(statuses))
	for key, status := range statuses {
		if key == "agent" || status.Ready {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		details = append(details, formatAssetStatusDetail(statuses[key], filepath.Base(key)))
	}
	percent := ready * 100 / total
	if len(details) == 0 {
		return fmt.Sprintf("Pre-staging assets in background: %d/%d ready (%d%%)", ready, total, percent)
	}
	return fmt.Sprintf("Pre-staging assets in background: %d/%d ready (%d%%; %s)", ready, total, percent, strings.Join(details, "; "))
}

func (m launchModel) launchPhaseLabel(groupIndex int, phase string) string {
	if groupIndex >= 0 && strings.HasPrefix(phase, "staging (") {
		if groupIndex < len(m.groups) {
			if enriched := m.formatAssetProgressForDirs(m.groups[groupIndex].SourceDirs()); enriched != "" {
				return enriched
			}
		}
	}
	return phase
}

func (m launchModel) launchElapsedLine() string {
	if m.launchStartedAt.IsZero() {
		return ""
	}
	elapsed := time.Since(m.launchStartedAt).Truncate(time.Second)
	if elapsed < time.Second {
		elapsed = time.Second
	}
	return fmt.Sprintf("elapsed: %s", elapsed)
}

func (m launchModel) launchKnownInstancesLine() string {
	known := len(m.registeredInstanceIDs)
	if m.expectedInstanceCount > 0 {
		return fmt.Sprintf("new instances discovered: %d/%d", known, m.expectedInstanceCount)
	}
	return fmt.Sprintf("new instances discovered: %d", known)
}

func formatLaunchStatusCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	remaining := make(map[string]int, len(counts))
	for status, count := range counts {
		remaining[status] = count
	}
	orderedStatuses := []string{
		db.LaunchStatusPlanned,
		db.LaunchStatusLaunching,
		db.LaunchStatusRunning,
		db.LaunchStatusGrace,
		db.LaunchStatusCompleted,
		db.LaunchStatusFailed,
		db.LaunchStatusCancelled,
	}
	parts := make([]string, 0, len(counts))
	for _, status := range orderedStatuses {
		count, ok := remaining[status]
		if !ok || count == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d", status, count))
		delete(remaining, status)
	}
	if len(remaining) > 0 {
		extra := make([]string, 0, len(remaining))
		for status := range remaining {
			extra = append(extra, status)
		}
		sort.Strings(extra)
		for _, status := range extra {
			parts = append(parts, fmt.Sprintf("%s %d", status, remaining[status]))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "instance states: " + strings.Join(parts, ", ")
}

func (m launchModel) launchStallLine() string {
	if m.lastLaunchProgressAt.IsZero() {
		return ""
	}
	stalledFor := time.Since(m.lastLaunchProgressAt)
	if stalledFor < launchStallThreshold {
		return ""
	}
	return fmt.Sprintf("No new-instance launch callbacks for %s; checking DB state...", stalledFor.Truncate(time.Second))
}

func (m launchModel) renderInlineLaunchOverview() string {
	if m.inlineWatch == nil || !m.launching {
		return ""
	}
	var b strings.Builder
	spinnerText := m.spinner.View()
	if m.inlineWatch != nil {
		spinnerText = m.inlineWatch.spinner.View()
	}
	b.WriteString(spinnerText)
	if m.campaignID != 0 {
		b.WriteString(fmt.Sprintf(" Launching instances... (campaign %d)", m.campaignID))
	} else {
		b.WriteString(" Launching instances...")
	}
	b.WriteString("\n")

	pendingLines := 0
	if elapsed := m.launchElapsedLine(); elapsed != "" {
		b.WriteString(fmt.Sprintf("  · %s\n", elapsed))
		pendingLines++
	}
	b.WriteString(fmt.Sprintf("  · %s\n", m.launchKnownInstancesLine()))
	pendingLines++
	if stateLine := formatLaunchStatusCounts(m.launchStatusCounts); stateLine != "" {
		b.WriteString(fmt.Sprintf("  · %s\n", stateLine))
		pendingLines++
	}
	if stall := m.launchStallLine(); stall != "" {
		b.WriteString("  · " + launchErrStyle.Render(stall) + "\n")
		pendingLines++
	}
	if m.launchHeartbeatErr != nil {
		b.WriteString("  · " + launchErrStyle.Render(fmt.Sprintf("DB launch refresh failed: %v", m.launchHeartbeatErr)) + "\n")
		pendingLines++
	}
	if m.campaignPhase != "" {
		b.WriteString(fmt.Sprintf("  · %s\n", m.campaignPhase))
		pendingLines++
	}
	for _, idx := range sortedIntKeys(m.groupPhases, m.groupDone) {
		if m.groupDone[idx] {
			continue
		}
		spec := fmt.Sprintf("group %d", idx)
		if idx >= 0 && idx < len(m.groups) {
			spec = m.groups[idx].GPUSpec()
		}
		phase := m.launchPhaseLabel(idx, m.groupPhases[idx])
		b.WriteString(fmt.Sprintf("  · %s: %s\n", spec, phase))
		pendingLines++
	}
	if pendingLines > 0 {
		b.WriteString("\n")
		return b.String()
	}
	return b.String()
}

func (m launchModel) renderInlineWatchView() string {
	if m.inlineWatch == nil {
		return ""
	}
	prefix := ""
	if m.err != nil {
		prefix += launchErrStyle.Render(fmt.Sprintf("Launch error: %v", m.err)) + "\n\n"
	}
	prefix += m.renderInlineLaunchOverview()
	if m.launching && len(m.registeredInstanceIDs) == 0 {
		return prefix
	}
	prefixLines := countRenderedLines(prefix)

	watch := *m.inlineWatch
	if m.width > 0 {
		watch.width = m.width
	}
	if m.height > 0 {
		watch.height = max(1, m.height-prefixLines)
	}
	return prefix + watch.View()
}

func (m launchModel) View() string {
	var b strings.Builder

	if m.err != nil && m.inlineWatch == nil {
		b.WriteString(launchErrStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
		return b.String()
	}
	if m.statusHint != "" && m.inlineWatch == nil {
		b.WriteString(launchDimStyle.Render(m.statusHint))
		b.WriteString("\n")
	}
	if m.inlineWatch != nil {
		b.WriteString(m.renderInlineWatchView())
		return b.String()
	}

	if m.done {
		if m.campaignID == 0 && m.expectedInstanceCount == 0 && len(m.instanceIDs) > 0 {
			b.WriteString("Submitted jobs to existing instances: ")
		} else if m.campaignID != 0 {
			b.WriteString(fmt.Sprintf("Campaign %d: launched instances: ", m.campaignID))
		} else {
			b.WriteString("Launched instances: ")
		}
		for i, id := range m.instanceIDs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(fmt.Sprintf("%d", id))
		}
		b.WriteString("\n")
		if len(m.partialErrors) > 0 {
			b.WriteString("\n")
			b.WriteString(formatPartialErrors(m.partialErrors, m.width))
			b.WriteString("\nPress Enter, Esc, or q to continue.\n")
		}
		return b.String()
	}

	if m.launching {
		b.WriteString(m.spinner.View())
		if m.campaignID != 0 {
			b.WriteString(fmt.Sprintf(" Launching instances... (campaign %d)", m.campaignID))
		} else {
			b.WriteString(" Launching instances...")
		}
		// Show campaign-level phase only when no per-group phases exist yet
		if len(m.groupPhases) == 0 && m.campaignPhase != "" {
			b.WriteString(fmt.Sprintf(" (%s)", m.campaignPhase))
		}
		b.WriteString("\n")
		if elapsed := m.launchElapsedLine(); elapsed != "" {
			b.WriteString("  · ")
			b.WriteString(elapsed)
			b.WriteString("\n")
		}
		b.WriteString("  · ")
		b.WriteString(m.launchKnownInstancesLine())
		b.WriteString("\n")
		if stateLine := formatLaunchStatusCounts(m.launchStatusCounts); stateLine != "" {
			b.WriteString("  · ")
			b.WriteString(stateLine)
			b.WriteString("\n")
		}
		if stall := m.launchStallLine(); stall != "" {
			b.WriteString("  · ")
			b.WriteString(launchErrStyle.Render(stall))
			b.WriteString("\n")
		}
		if m.launchHeartbeatErr != nil {
			b.WriteString("  · ")
			b.WriteString(launchErrStyle.Render(fmt.Sprintf("DB launch refresh failed: %v", m.launchHeartbeatErr)))
			b.WriteString("\n")
		}
		// Per-group progress lines
		for _, idx := range sortedIntKeys(m.groupPhases, m.groupDone) {
			spec := fmt.Sprintf("group %d", idx)
			if idx >= 0 && idx < len(m.groups) {
				spec = m.groups[idx].GPUSpec()
			}
			if m.groupDone[idx] {
				b.WriteString(fmt.Sprintf("  ✓ %s\n", spec))
			} else {
				phase := m.launchPhaseLabel(idx, m.groupPhases[idx])
				b.WriteString(fmt.Sprintf("  · %s: %s\n", spec, phase))
			}
		}
		return b.String()
	}

	totalJobs := countGroupJobs(m.groups)

	b.WriteString(launchTitleStyle.Render(fmt.Sprintf("Rental GPU jobs (%d jobs, %d GPU groups)", totalJobs, len(m.groups))))
	// Show loading/reconciling status inline after the title
	if m.reconciling || m.loading {
		var status []string
		if m.reconciling {
			status = append(status, "reconciling")
		}
		if m.fetchingOffers() {
			status = append(status, "fetching offers")
		} else if m.buildingPlans() {
			status = append(status, "planning launch options")
		} else if m.loading {
			status = append(status, "loading")
		}
		b.WriteString("  ")
		b.WriteString(m.spinner.View())
		b.WriteString(launchDimStyle.Render(strings.Join(status, ", ") + "..."))
	}
	b.WriteString("\n")
	if m.reconcileDropped > 0 {
		b.WriteString(launchDimStyle.Render(fmt.Sprintf("  %s resolved during sync (completed or failed on previous instance)", campaign.PluralJobs(m.reconcileDropped))))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if assetSummary := m.formatBackgroundAssetSummary(); assetSummary != "" {
		b.WriteString(launchDimStyle.Render(assetSummary))
		b.WriteString("\n\n")
	}

	selected := m.selectedCountByGroup()

	if m.showCostDetail && m.costEstimates != nil {
		// Detail view replaces job list
		b.WriteString(campaign.FormatCostBreakdown(m.costEstimates, selected))
	} else {
		// Job list with checkboxes (windowed by offset/pageSize)
		pageSize := m.pageSize()
		endIdx := m.offset + pageSize
		if endIdx > len(m.items) {
			endIdx = len(m.items)
		}

		if m.offset > 0 {
			b.WriteString(launchDimStyle.Render(fmt.Sprintf("  ↑ %d more above", m.offset)))
			b.WriteString("\n")
		}

		for idx := m.offset; idx < endIdx; idx++ {
			item := m.items[idx]
			isCursor := idx == m.cursor

			noOffer := m.offersLoaded() && !m.groupHasOffer(item.groupIdx)

			if item.isHeader {
				checkbox := m.groupCheckState(item.groupIdx)
				line := fmt.Sprintf("%s %s", checkbox, item.label)
				var style lipgloss.Style
				if noOffer {
					line += " (no offers)"
					style = launchNoOfferStyle
				} else {
					style = launchHeaderStyle
				}
				renderRow(&b, style, line, isCursor)
				b.WriteString("\n")
				continue
			}

			checked := m.selected[item.jobID]

			var checkbox string
			if checked {
				checkbox = "[x]"
			} else {
				checkbox = "[ ]"
			}

			line := fmt.Sprintf("%s %s", checkbox, item.label)

			var style lipgloss.Style
			if noOffer {
				style = launchNoOfferStyle
			} else if checked {
				style = launchSelectedStyle
			} else {
				style = launchDimStyle
			}
			renderRow(&b, style, line, isCursor)
			b.WriteString("\n")
		}

		if endIdx < len(m.items) {
			b.WriteString(launchDimStyle.Render(fmt.Sprintf("  ↓ %d more below", len(m.items)-endIdx)))
			b.WriteString("\n")
		}

		// Cost estimate table
		if m.costEstimates != nil {
			b.WriteString("\n")
			if m.cachedRawOffers != nil && m.cachedTradeoffRows != nil {
				// Tradeoff comparison view
				b.WriteString(launchDimStyle.Render("── Cost Estimate (s tradeoff  ←/→ details) ────────────"))
				b.WriteString("\n")
				rows := m.cachedTradeoffRows
				summaryTable := campaign.FormatStrategySummary(rows)
				b.WriteString(launchDimStyle.Render(formatTradeoffColumnHeader(summaryTable)))
				b.WriteString("\n")

				// Build final lines, splicing detail rows if disclosed
				var finalLines []campaign.CostLine
				for i, line := range summaryTable.Lines {
					finalLines = append(finalLines, line)
					if m.tradeoffDisclosed && i < len(rows) && rows[i].Active && rows[i].Disclosed {
						activeEstimates := m.cachedActiveEstimates
						if activeEstimates != nil {
							indent := strings.Repeat(" ", summaryTable.TimeColOffset)
							// When the winning candidate differs from split groups,
							// pass nil for selection (all jobs selected) since the
							// estimates use the candidate's group indices.
							detailSelected := selected
							if plan, ok := m.activeTradeoffPlan(); ok && plan.HasComplexExecution() {
								detailSelected = nil
							}
							detailTable := campaign.FormatCostTableSelected(activeEstimates, detailSelected, summaryTable.TimeWidth, summaryTable.RateWidth)
							detailLines := detailTable.Lines
							if len(detailLines) > 1 {
								detailLines = detailLines[:len(detailLines)-1]
							}
							for _, dl := range detailLines {
								dl.Text = indent + dl.Text
								finalLines = append(finalLines, dl)
							}
						}
					}
				}

				// Render strategy lines with cursor awareness
				for i, cl := range finalLines {
					isCursor := m.focusArea == focusTradeoffs && i < len(rows) && i == m.tradeoffCursor
					text := cl.Text
					if isCursor {
						// Replace 2-rune prefix (▸ /▾ /  ) with cursor indicator
						runes := []rune(text)
						if len(runes) >= 2 {
							text = "> " + string(runes[2:])
						}
					}
					style := launchDimStyle
					if isCursor {
						style = launchSelectedStyle.Bold(true)
					} else if !cl.Dimmed {
						style = launchSelectedStyle
					}
					b.WriteString(style.Render(text))
					b.WriteString("\n")
				}

				if sparkTable := campaign.FormatParetoSparkline(rows); sparkTable != nil {
					b.WriteString("\n")
					renderCostTable(&b, *sparkTable)
				}
				if hint := m.activeTradeoffPredictionHint(selected); hint != "" {
					b.WriteString(launchDimStyle.Render(hint))
					b.WriteString("\n")
				}
			} else {
				// Fallback: single-strategy table
				b.WriteString(costEstimateHeader(false))
				b.WriteString("\n")
				costTable := campaign.FormatCostTableSelected(m.costEstimates, selected, 0, 0)
				renderCostTable(&b, costTable)
				if hint := campaign.SummarizeRuntimePredictions(m.costEstimates, selected).Hint(); hint != "" {
					b.WriteString(launchDimStyle.Render(hint))
					b.WriteString("\n")
				}
			}
		} else if m.fetchingOffers() {
			b.WriteString("\n")
			b.WriteString(costEstimateHeader(false))
			b.WriteString("\n")
			b.WriteString(m.spinner.View())
			b.WriteString(launchDimStyle.Render(" Searching providers for direct offers..."))
			b.WriteString("\n")
		} else if m.buildingPlans() {
			b.WriteString("\n")
			b.WriteString(costEstimateHeader(false))
			b.WriteString("\n")
			renderRawOfferSummary(&b, m.cachedRawOffers)
			b.WriteString(m.spinner.View())
			b.WriteString(launchDimStyle.Render(" Building launch plan from raw offers..."))
			b.WriteString("\n")
		} else if m.groupOffers != nil {
			b.WriteString("\n")
			b.WriteString(costEstimateHeader(true))
			b.WriteString("\n")
			costTable := campaign.FormatCostTable(m.groupOffers, selected)
			renderCostTable(&b, costTable)
			b.WriteString(m.spinner.View())
			if m.estimateProgress.phase != "" {
				b.WriteString(launchDimStyle.Render(fmt.Sprintf(" %s (%d/%d)...", m.estimateProgress.phase, m.estimateProgress.resolved, m.estimateProgress.total)))
			} else {
				b.WriteString(launchDimStyle.Render(" Computing estimates..."))
			}
			b.WriteString("\n")
		}
	}

	// Skipped-job warning
	b.WriteString("\n")
	selectedCount := 0
	for _, v := range m.selected {
		if v {
			selectedCount++
		}
	}
	if m.offersLoaded() && !m.showCostDetail {
		skippedCount := selectedCount - m.selectedLaunchableJobCount()
		if skippedCount > 0 {
			b.WriteString(launchErrStyle.Render(fmt.Sprintf("  %d selected job(s) will be skipped (no offers)", skippedCount)))
			b.WriteString("\n")
		}
	}

	// Help line
	var help string
	if m.showCostDetail {
		help = "d back  q quit"
	} else if selectedCount == 0 {
		help = "↑/↓ navigate  space toggle  s tradeoff  j jump  ←/→ expand  d details  enter quit  q quit"
	} else {
		help = "↑/↓ navigate  space toggle  s tradeoff  j jump  ←/→ expand  d details  enter launch  q quit"
	}
	b.WriteString(launchDimStyle.Render(help))
	b.WriteString("\n")

	return b.String()
}

// sortedIntKeys returns the union of keys from groupPhases and groupDone, sorted ascending.
func sortedIntKeys(phases map[int]string, done map[int]bool) []int {
	seen := make(map[int]struct{})
	for k := range phases {
		seen[k] = struct{}{}
	}
	for k := range done {
		seen[k] = struct{}{}
	}
	keys := make([]int, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
