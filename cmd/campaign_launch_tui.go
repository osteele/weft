package cmd

import (
	"database/sql"
	"fmt"
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

// formatPartialErrors renders a list of launch failure messages.
func formatPartialErrors(errors []string) string {
	var b strings.Builder
	b.WriteString(launchErrStyle.Render(fmt.Sprintf("%d planned launch(es) failed:", len(errors))))
	b.WriteString("\n")
	for _, errMsg := range errors {
		b.WriteString("  - ")
		b.WriteString(errMsg)
		b.WriteString("\n")
	}
	return b.String()
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
	focusJobs       launchFocusArea = iota // cursor is in the job list
	focusStrategies                        // cursor is in the strategy comparison
)

type launchModel struct {
	groups          []campaign.InstanceGroup
	groupOffers     []campaign.GroupOffer
	cachedRawOffers []campaign.GroupRawOffers // cached raw offers for the split (base) groups
	strategyPlans   map[bidding.SelectionStrategy]campaign.StrategyPlan
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
	strategyDisclosed bool // true = show detail rows for active strategy inline

	// Focus state for unified jobs+strategies navigation.
	focusArea      launchFocusArea // which area has cursor focus
	strategyCursor int             // index into dedupedStrategies (persists when in job area)

	// dedupedStrategies is the list of visually distinct strategies (after
	// merging those with identical offer sets). The 's' key cycles through
	// this list. Rebuilt whenever offers are re-ranked.
	dedupedStrategies []bidding.SelectionStrategy

	// winningCandidate caches the best candidate result for the active strategy.
	// Used by launchInstances to get the actual groups/offers for launch
	// (which may differ from m.groups when the merged candidate wins).
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
	phaseCh          chan launchPhaseMsg
	instanceCh       chan launchInstanceRegisteredMsg

	instanceIDs             []int64
	expectedInstanceCount   int
	inlineWatchEnabled      bool
	inlineWatchUsed         bool
	registeredInstanceIDs   []int64
	registeredInstanceIDSet map[int64]struct{}
	inlineWatch             *watchModel
	database                *sql.DB
	clients                 []cloud.Client
	providerErr             error
	appConfig               *config.Config
	launchOpts              campaign.LaunchOpts
	gpuFilter               string // --gpu filter to reapply after reconciliation
	reconciler              *campaign.Reconciler
	reconcileDropped        int

	// Cached strategy display data — recomputed only when strategyRowsDirty.
	cachedStrategyRows    []campaign.StrategySummaryRow
	cachedActiveEstimates []campaign.CostEstimate
	strategyRowsDirty     bool

	spinner spinner.Model
	width   int
	height  int
}

// Messages
type reconcileDoneMsg struct {
	groups []campaign.InstanceGroup
}

type rawOffersLoadedMsg struct {
	raw        []campaign.GroupRawOffers
	plans      map[bidding.SelectionStrategy]campaign.StrategyPlan
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

type campaignCreatedMsg struct {
	campaignID int64
}

type launchPhaseMsg struct {
	groupIndex int // -1 for campaign-level phases
	phase      string
}

type launchInstanceRegisteredMsg struct {
	instanceID int64
	groupIndex int
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

func newLaunchModel(database *sql.DB, clients []cloud.Client, providerErr error, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts, predCfg *predictor.Config, gpuFilter string, reconciling bool, fromWatch bool, inlineWatchEnabled bool) launchModel {
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
		phaseCh:                 make(chan launchPhaseMsg, 16),
		instanceCh:              make(chan launchInstanceRegisteredMsg, len(groups)),
		gpuFilter:               gpuFilter,
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
	clients := m.clients
	cfg := m.appConfig
	gpuFilter := m.gpuFilter
	reconciler := m.reconciler
	return func() tea.Msg {
		r2Client, _ := buildR2Client(cfg)
		syncCloudStateWithClients(cfg, database, reconciler, clients, r2Client, false)

		// Re-query unplaced jobs since reconciliation may have freed some
		jobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return reconcileDoneMsg{}
		}
		jobs = filterRentalLaunchJobs(jobs)
		jobs = filterLaunchJobsByProject(jobs)

		groups := campaign.PrepareGroups(jobs, database, gpuFilter, r2Client)

		return reconcileDoneMsg{groups: groups}
	}
}

func (m launchModel) fetchRawOffers(background bool) tea.Cmd {
	groups := m.groups
	clients := m.clients
	providerErr := m.providerErr
	database := m.database
	predCfg := m.predConfig
	overheadModel := m.overheadModel
	survivalModel := m.survivalModel
	return func() tea.Msg {
		reusable, _ := campaign.FindReusableInstances(database)
		if len(clients) == 0 && len(reusable) == 0 {
			err := providerErr
			if err == nil {
				err = fmt.Errorf("no rental providers available")
			}
			return rawOffersLoadedMsg{err: err}
		}
		plans, splitRaw := campaign.BuildStrategyPlans(
			database,
			clients,
			groups,
			reusable,
			predCfg,
			overheadModel,
			survivalModel,
			allStrategies,
			m.launchOpts.MinSurvival,
		)
		return rawOffersLoadedMsg{raw: splitRaw, plans: plans, background: background}
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

// rankCachedOffersForStrategy ranks cached raw offers with a specific strategy.
// When candidate groupings are available, selects the best candidate and maps
// its offers back to m.groups for display alignment.
func (m launchModel) rankCachedOffersForStrategy(strategy bidding.SelectionStrategy) []campaign.GroupOffer {
	if plan, ok := m.strategyPlanFor(strategy); ok {
		return plan.DisplayOffers
	}
	return nil
}

// rankCachedOffers ranks cached raw offers with the current strategy and
// caches the winning candidate result for launch.
func (m *launchModel) rankCachedOffers() []campaign.GroupOffer {
	plan, ok := m.strategyPlanFor(m.launchOpts.Strategy)
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

// activeStrategyEstimates returns the cost estimates for the active strategy.
// When a non-split candidate wins (e.g. "parallel"), returns approximate
// estimates built from the candidate's actual group offers, so the detail
// rows reflect the winning grouping (per-instance breakdown).
func (m launchModel) activeStrategyEstimates() []campaign.CostEstimate {
	plan, ok := m.strategyPlanFor(m.launchOpts.Strategy)
	if !ok {
		return nil
	}
	if plan.HasComplexExecution() {
		return plan.ActualEstimates
	}
	return plan.DisplayEstimates
}

func (m launchModel) strategyPlanFor(strategy bidding.SelectionStrategy) (campaign.StrategyPlan, bool) {
	if m.strategyPlans == nil {
		return campaign.StrategyPlan{}, false
	}
	plan, ok := m.strategyPlans[strategy]
	return plan, ok
}

// allStrategies returns the three bidding strategies in cycle order.
var allStrategies = []bidding.SelectionStrategy{
	bidding.StrategyCheap,
	bidding.StrategyFast,
	bidding.StrategyFastest,
}

// preflightOtherStrategies launches background estimate fetches for strategies
// whose offer sets are not already in the cache.
func (m launchModel) preflightOtherStrategies() []tea.Cmd {
	return nil
}

// rebuildDedupedStrategies computes the list of visually distinct strategies by
// deduplicating those that produce identical offer sets. The first strategy in
// each group of duplicates is kept; later ones are dropped.
func (m *launchModel) rebuildDedupedStrategies() {
	if m.cachedRawOffers == nil {
		m.dedupedStrategies = append([]bidding.SelectionStrategy(nil), allStrategies...)
		return
	}
	seen := make(map[string]bool)
	m.dedupedStrategies = m.dedupedStrategies[:0]
	for _, strat := range allStrategies {
		offers := m.rankCachedOffersForStrategy(strat)
		key := offerIdentity(offers)
		if seen[key] {
			continue
		}
		seen[key] = true
		m.dedupedStrategies = append(m.dedupedStrategies, strat)
	}
}

// visibleStrategies returns the list of strategies that are currently displayed.
// This is the deduped list when available, otherwise the full list.
func (m launchModel) visibleStrategies() []bidding.SelectionStrategy {
	if len(m.dedupedStrategies) > 0 {
		return m.dedupedStrategies
	}
	return allStrategies
}

// activateStrategyIfFocused sets the active strategy to the one under the
// strategy cursor and re-ranks offers. No-op if focus is on jobs.
func (m *launchModel) activateStrategyIfFocused() {
	if m.focusArea != focusStrategies {
		return
	}
	strategies := m.visibleStrategies()
	if m.strategyCursor >= len(strategies) {
		m.strategyCursor = len(strategies) - 1
	}
	if m.strategyCursor < 0 {
		return
	}
	newStrategy := strategies[m.strategyCursor]
	if newStrategy == m.launchOpts.Strategy {
		return
	}
	m.launchOpts.Strategy = newStrategy
	m.strategyRowsDirty = true
	if m.cachedRawOffers != nil {
		m.groupOffers = m.rankCachedOffers()
		m.costEstimates = m.activeStrategyEstimates()
		if m.costEstimates == nil {
			m.costEstimates = m.estimateCache[offerIdentity(m.groupOffers)]
		}
	}
}

// fetchEstimatesIfNeeded returns a tea.Cmd that fetches cost estimates if the
// current strategy's estimates aren't cached yet. Returns nil if estimates are
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

// gatherStrategyRows builds a StrategySummaryRow for each strategy using cached estimates.
// Strategies that resolve to the same offers are merged into a single row (e.g. "fast/fastest").
//
// When a non-split candidate wins (e.g. "parallel" or "merged"), the summary
// is built from the candidate's actual group offers rather than the split-mapped
// estimates, so instance counts and time/cost reflect the winning grouping.
func (m launchModel) gatherStrategyRows(selectedPerGroup []int) []campaign.StrategySummaryRow {
	var rows []campaign.StrategySummaryRow
	keyToIdx := make(map[string]int) // offerIdentity → index into rows

	for _, strat := range allStrategies {
		isActive := strat == m.launchOpts.Strategy
		offers := m.rankCachedOffersForStrategy(strat)
		key := offerIdentity(offers)
		plan, havePlan := m.strategyPlanFor(strat)

		// Check if we already have a row for the same offer set
		if idx, ok := keyToIdx[key]; ok {
			rows[idx].Label += "/" + string(strat)
			if isActive {
				rows[idx].Active = true
				rows[idx].Disclosed = m.strategyDisclosed
			}
			continue
		}

		row := campaign.StrategySummaryRow{
			Label:     string(strat),
			Active:    isActive,
			Disclosed: isActive && m.strategyDisclosed,
			Loading:   true,
		}

		// Try detailed estimates first when the plan aligns with split groups.
		if havePlan && !plan.HasComplexExecution() {
			if summary := campaign.SummarizeForComparison(plan.DisplayEstimates, selectedPerGroup); summary != nil {
				row = *summary
				row.Label = string(strat)
				row.Active = isActive
				row.Disclosed = isActive && m.strategyDisclosed
			}
		} else if cached, ok := m.estimateCache[key]; ok {
			if summary := campaign.SummarizeForComparison(cached, selectedPerGroup); summary != nil {
				row = *summary
				row.Label = string(strat)
				row.Active = isActive
				row.Disclosed = isActive && m.strategyDisclosed
			}
		}

		if havePlan && plan.HasComplexExecution() {
			if summary := campaign.SummarizeExecutionEstimates(plan.ActualEstimates); summary != nil {
				summary.Label = row.Label
				summary.Active = row.Active
				summary.Disclosed = row.Disclosed
				row = *summary
			}
		}

		keyToIdx[key] = len(rows)
		rows = append(rows, row)
	}
	return rows
}

// refreshStrategyRowsIfNeeded recomputes cached strategy display data
// when the underlying data has changed. Call from Update(), not View().
func (m *launchModel) refreshStrategyRowsIfNeeded() {
	if !m.strategyRowsDirty && m.cachedStrategyRows != nil {
		return
	}
	selected := m.selectedCountByGroup()
	m.cachedStrategyRows = m.gatherStrategyRows(selected)
	m.cachedActiveEstimates = m.activeStrategyEstimates()
	m.strategyRowsDirty = false
}

func (m launchModel) fetchEstimatesForOffers(offers []campaign.GroupOffer, cacheKey string, reportProgress bool) tea.Cmd {
	predConfig := m.predConfig
	ch := m.progressCh
	overheadModel := m.overheadModel
	survivalModel := m.survivalModel
	referenceDLPerf := campaign.MedianDLPerfFromRawOffers(m.cachedRawOffers)
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
		estimates := campaign.EstimateCosts(m.database, offers, predConfig, overheadModel, nil, survivalModel, referenceDLPerf, onProgress)
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

func waitForLaunchInstanceRegistered(ch chan launchInstanceRegisteredMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
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
	if !m.inlineWatchEnabled || m.inlineWatch != nil || m.expectedInstanceCount == 0 {
		return m, nil
	}
	if len(m.registeredInstanceIDs) < m.expectedInstanceCount {
		return m, nil
	}
	var r2Client *r2.Client
	if m.appConfig != nil {
		r2Client, _ = buildR2Client(m.appConfig)
	}
	inlineWatch := newInstanceWatchModel(m.database, append([]int64(nil), m.registeredInstanceIDs...), r2Client, m.appConfig)
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
	if m.inlineWatch != nil {
		switch msg.(type) {
		case campaignCreatedMsg, launchPhaseMsg, launchInstanceRegisteredMsg, instancesLaunchedMsg:
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
			lm.refreshStrategyRowsIfNeeded()
			return lm, cmd
		}
		return result, cmd

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.adjustOffset()
		return m, nil

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
		if msg.groups == nil {
			return m, nil // reconciliation failed, keep current state
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
			m.strategyPlans = nil
			m.reuseAssignments = nil
			m.winningCandidate = nil
			m.dedupedStrategies = nil
			m.costEstimates = nil
			m.estimateCache = make(map[string][]campaign.CostEstimate)
			m.strategyRowsDirty = true
			m.cachedStrategyRows = nil
			return m, m.fetchRawOffers(false)
		}
		return m, nil

	case rawOffersLoadedMsg:
		if msg.err != nil {
			m.loading = false
			m.err = msg.err
			return m, m.quitOrSwitchToWatch(fmt.Sprintf("Launch error: %v", msg.err))
		}
		m.cachedRawOffers = msg.raw
		m.strategyPlans = msg.plans
		for _, plan := range msg.plans {
			if len(plan.DisplayOffers) == 0 {
				continue
			}
			m.estimateCache[offerIdentity(plan.DisplayOffers)] = plan.DisplayEstimates
		}
		m.strategyRowsDirty = true
		m.rebuildDedupedStrategies()
		oldOffers := m.groupOffers
		m.groupOffers = m.rankCachedOffers()
		if !msg.background {
			m.loading = false
		}
		key := offerIdentity(m.groupOffers)
		if offersMatch(oldOffers, m.groupOffers) && m.costEstimates != nil {
			m.refreshStrategyRowsIfNeeded()
			return m, nil
		}
		m.costEstimates = m.activeStrategyEstimates()
		if m.costEstimates == nil {
			m.costEstimates = m.estimateCache[key]
		}
		m.refreshStrategyRowsIfNeeded()
		return m, nil

	case estimateProgressMsg:
		m.estimateProgress = msg
		if m.costEstimates == nil {
			return m, waitForProgress(m.progressCh)
		}
		return m, nil

	case estimatesLoadedMsg:
		m.estimateCache[msg.cacheKey] = msg.estimates
		m.strategyRowsDirty = true
		// Only update display if this is for the current strategy's offers
		if msg.cacheKey == offerIdentity(m.groupOffers) {
			m.costEstimates = msg.estimates
		}
		m.refreshStrategyRowsIfNeeded()
		return m, nil

	case campaignCreatedMsg:
		m.campaignID = msg.campaignID
		return m, nil

	case launchPhaseMsg:
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

	case launchInstanceRegisteredMsg:
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
		next, watchCmd := m.maybeStartInlineWatch()
		m = next
		if watchCmd != nil {
			cmds = append(cmds, watchCmd)
		}
		if len(cmds) == 0 {
			return m, nil
		}
		return m, tea.Batch(cmds...)

	case instancesLaunchedMsg:
		m.launching = false
		if msg.err != nil {
			m.err = msg.err
			if m.inlineWatch != nil {
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
			return m, nil
		}
		if len(m.partialErrors) == 0 {
			if m.campaignID == 0 && m.winningCandidate == nil && len(m.instanceIDs) > 0 {
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
		return m, nil // ignore keys while launching
	}

	switch msg.String() {
	case "enter":
		if m.done {
			return m, m.quitOrSwitchToWatch(formatLaunchResultFlash(m.instanceIDs, m.partialErrors))
		}
		if m.loading || m.reconciling {
			status := "Loading offers"
			if m.reconciling {
				status = "Reconciling"
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
		m.expectedInstanceCount = m.selectedNewLaunchGroupCount()
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
		return m, tea.Batch(
			m.spinner.Tick,
			m.launchInstances(),
			m.waitForCampaignCreated(),
			waitForLaunchPhase(m.phaseCh),
			waitForLaunchInstanceRegistered(m.instanceCh),
		)

	case "q", "esc", "ctrl+c":
		return m, m.quitOrSwitchToWatch("Launch canceled.")

	case "up", "k":
		if m.focusArea == focusJobs {
			if m.cursor > 0 {
				m.cursor--
			} else {
				// Wrap to bottom of strategies
				strats := m.visibleStrategies()
				if len(strats) > 0 {
					m.focusArea = focusStrategies
					m.strategyCursor = len(strats) - 1
					m.activateStrategyIfFocused()
				}
			}
		} else {
			if m.strategyCursor > 0 {
				m.strategyCursor--
				m.activateStrategyIfFocused()
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
				// Move to top of strategies
				strats := m.visibleStrategies()
				if len(strats) > 0 {
					m.focusArea = focusStrategies
					m.strategyCursor = 0
					m.activateStrategyIfFocused()
				}
			}
		} else {
			strats := m.visibleStrategies()
			if m.strategyCursor < len(strats)-1 {
				m.strategyCursor++
				m.activateStrategyIfFocused()
			} else {
				// Wrap to top of jobs
				m.focusArea = focusJobs
				m.cursor = 0
			}
		}
		m.adjustOffset()
		return m, m.fetchEstimatesIfNeeded()

	case "j":
		// Jump between jobs and strategies
		if m.focusArea == focusJobs {
			strats := m.visibleStrategies()
			if len(strats) > 0 {
				m.focusArea = focusStrategies
				m.activateStrategyIfFocused()
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
			m.strategyRowsDirty = true
		}
		// Space in strategies area is a no-op (strategy is already active)
		return m, nil

	case "d":
		m.showCostDetail = !m.showCostDetail
		return m, nil

	case "right":
		if m.focusArea == focusStrategies {
			m.strategyDisclosed = true
			m.strategyRowsDirty = true
		}
		return m, nil

	case "left":
		if m.focusArea == focusStrategies {
			m.strategyDisclosed = false
			m.strategyRowsDirty = true
		}
		return m, nil

	case "a":
		for id := range m.selected {
			m.selected[id] = true
		}
		m.strategyRowsDirty = true
		return m, nil

	case "n":
		for id := range m.selected {
			m.selected[id] = false
		}
		m.strategyRowsDirty = true
		return m, nil

	case "s":
		// Switch to strategies area and cycle among them
		strategies := m.visibleStrategies()
		if len(strategies) == 0 {
			return m, nil
		}
		if m.focusArea != focusStrategies {
			// Enter strategies area at the current active strategy
			m.focusArea = focusStrategies
			for i, s := range strategies {
				if s == m.launchOpts.Strategy {
					m.strategyCursor = i
					break
				}
			}
		}
		// Cycle to next strategy (with wrap)
		m.strategyCursor = (m.strategyCursor + 1) % len(strategies)
		m.activateStrategyIfFocused()
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
	// Separate selected reuse assignments from new-instance launches.
	var selectedReuse []campaign.ReuseAssignment
	for _, assignment := range m.reuseAssignments {
		if m.selected[assignment.Job.ID] {
			selectedReuse = append(selectedReuse, assignment)
		}
	}

	sourceGroups := []campaign.InstanceGroup(nil)
	sourceOffers := []campaign.GroupOffer(nil)
	sourceEstimates := []campaign.CostEstimate(nil)
	if m.winningCandidate != nil && len(m.winningCandidate.Groups) > 0 {
		sourceGroups = m.winningCandidate.Groups
		sourceOffers = m.winningCandidate.Offers
		sourceEstimates = m.winningCandidate.Estimates
	}

	var filteredGroups []campaign.InstanceGroup
	var filteredOffers []campaign.GroupOffer
	var filteredEstimates []campaign.CostEstimate
	for i, g := range sourceGroups {
		var selectedJobs []*db.Job
		for _, job := range g.Jobs {
			if m.selected[job.ID] {
				selectedJobs = append(selectedJobs, job)
			}
		}
		if len(selectedJobs) == 0 {
			continue
		}
		fg := campaign.InstanceGroup{
			GPUClass:    g.GPUClass,
			GPUMemGB:    g.GPUMemGB,
			MaxGPUMemGB: g.MaxGPUMemGB,
			DiskGB:      g.DiskGB,
			Image:       g.Image,
			Jobs:        selectedJobs,
		}
		filteredGroups = append(filteredGroups, fg)
		if i < len(sourceOffers) {
			filteredOffers = append(filteredOffers, sourceOffers[i])
		}
		if i < len(sourceEstimates) {
			scale := 1.0
			if totalJobs := len(g.Jobs); totalJobs > 0 {
				scale = float64(len(selectedJobs)) / float64(totalJobs)
			}
			scaled := sourceEstimates[i]
			scaled.Group = fg
			scaled.TotalTime = time.Duration(float64(sourceEstimates[i].TotalTime) * scale)
			scaled.TotalCost = sourceEstimates[i].TotalCost * scale
			scaled.Breakdown.Total = sourceEstimates[i].Breakdown.Total.Scale(scale)
			if i < len(sourceOffers) {
				scaled.Offer = sourceOffers[i]
				scaled.Offer.Group = fg
			}
			filteredEstimates = append(filteredEstimates, scaled)
		}
	}

	var launchGroups []campaign.InstanceGroup
	var offers []cloud.Offer
	var launchGroupOffers []campaign.GroupOffer
	var selectedEstimates []campaign.CostEstimate
	for i, fg := range filteredGroups {
		if i < len(filteredOffers) && filteredOffers[i].Offer != nil {
			offerCopy := *filteredOffers[i].Offer
			launchGroups = append(launchGroups, fg)
			offers = append(offers, offerCopy)
			launchGroupOffers = append(launchGroupOffers, campaign.GroupOffer{
				Group:        fg,
				Offer:        &offerCopy,
				SurvivalProb: filteredOffers[i].SurvivalProb,
			})
			if i < len(filteredEstimates) {
				scaled := filteredEstimates[i]
				scaled.Offer = launchGroupOffers[len(launchGroupOffers)-1]
				selectedEstimates = append(selectedEstimates, scaled)
			}
		}
	}

	database := m.database
	clients := m.clients
	cfg := m.appConfig
	opts := m.launchOpts
	campaignCh := m.campaignCh
	phaseCh := m.phaseCh
	instanceCh := m.instanceCh
	predCfg := m.predConfig
	overheadModel := m.overheadModel
	survivalModel := m.survivalModel
	referenceDLPerf := campaign.MedianDLPerfFromGroupOffers(launchGroupOffers)

	// Auto-derive budget limits from cached estimates if not set by CLI
	if (opts.MaxSpendCents == 0 || opts.MaxTimeSeconds == 0) && len(selectedEstimates) > 0 {
		opts.ApplyAutoBudget(selectedEstimates)
	}

	return func() tea.Msg {
		defer close(campaignCh)
		defer close(phaseCh)
		defer close(instanceCh)
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
			selectedEstimates = campaign.EstimateCosts(m.database, launchGroupOffers, predCfg, overheadModel, nil, survivalModel, referenceDLPerf, nil)
		}

		r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
		sendCampaignPhase("preparing campaign launch")

		result, err := campaign.LaunchCampaign(
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
		if m.err != nil {
			b.WriteString(launchErrStyle.Render(fmt.Sprintf("Launch error: %v", m.err)))
			b.WriteString("\n\n")
		}
		b.WriteString(m.inlineWatch.View())
		return b.String()
	}

	if m.done {
		if m.campaignID == 0 && m.winningCandidate == nil && len(m.instanceIDs) > 0 {
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
			b.WriteString(formatPartialErrors(m.partialErrors))
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
		// Per-group progress lines
		for _, idx := range sortedIntKeys(m.groupPhases, m.groupDone) {
			spec := fmt.Sprintf("group %d", idx)
			if idx >= 0 && idx < len(m.groups) {
				spec = m.groups[idx].GPUSpec()
			}
			if m.groupDone[idx] {
				b.WriteString(fmt.Sprintf("  ✓ %s\n", spec))
			} else {
				phase := m.groupPhases[idx]
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
		if m.loading {
			status = append(status, "fetching offers")
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
			if m.cachedRawOffers != nil && m.cachedStrategyRows != nil {
				// Strategy comparison view
				b.WriteString(launchDimStyle.Render("── Cost Estimate (s strategy  ←/→ details) ────────────"))
				b.WriteString("\n")
				rows := m.cachedStrategyRows
				summaryTable := campaign.FormatStrategySummary(rows)

				// Build final lines, splicing detail rows if disclosed
				var finalLines []campaign.CostLine
				for i, line := range summaryTable.Lines {
					finalLines = append(finalLines, line)
					if m.strategyDisclosed && i < len(rows) && rows[i].Active && rows[i].Disclosed {
						activeEstimates := m.cachedActiveEstimates
						if activeEstimates != nil {
							indent := strings.Repeat(" ", summaryTable.TimeColOffset)
							// When the winning candidate differs from split groups,
							// pass nil for selection (all jobs selected) since the
							// estimates use the candidate's group indices.
							detailSelected := selected
							if plan, ok := m.strategyPlanFor(m.launchOpts.Strategy); ok && plan.HasComplexExecution() {
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
					isCursor := m.focusArea == focusStrategies && i < len(rows) && i == m.strategyCursor
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
			} else {
				// Fallback: single-strategy table
				b.WriteString(costEstimateHeader(false))
				b.WriteString("\n")
				costTable := campaign.FormatCostTableSelected(m.costEstimates, selected, 0, 0)
				renderCostTable(&b, costTable)
			}
		} else if m.loading && m.groupOffers == nil {
			b.WriteString("\n")
			b.WriteString(costEstimateHeader(false))
			b.WriteString("\n")
			b.WriteString(m.spinner.View())
			b.WriteString(launchDimStyle.Render(" Awaiting offers..."))
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
		help = "↑/↓ navigate  space toggle  s strategy  j jump  ←/→ expand  d details  enter quit  q quit"
	} else {
		help = "↑/↓ navigate  space toggle  s strategy  j jump  ←/→ expand  d details  enter launch  q quit"
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
