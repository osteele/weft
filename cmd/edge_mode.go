package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

// edgeMode is the edge-role classification of one command. Every runnable
// command in the tree has exactly one mode; the tree-walk test in
// edge_mode_test.go keeps the table and the tree in lockstep.
type edgeMode int

const (
	// edgeModeLocal commands touch no ledger and run the same on any host.
	edgeModeLocal edgeMode = iota
	// edgeModeMirror commands render the hub's ledger. On an edge they read
	// the published hub view rather than a local database.
	edgeModeMirror
	// edgeModeSubmit commands ask the hub to act: they write a signed
	// payload to the edge inbox and the hub executes the same function the
	// local command would have called.
	edgeModeSubmit
	// edgeModeDisabled commands exercise hub authority and are refused by
	// role on an edge.
	edgeModeDisabled
)

func (m edgeMode) String() string {
	switch m {
	case edgeModeLocal:
		return "local"
	case edgeModeMirror:
		return "mirror"
	case edgeModeSubmit:
		return "submit"
	case edgeModeDisabled:
		return "disabled"
	}
	return fmt.Sprintf("edgeMode(%d)", int(m))
}

// Exit codes for the two blocked points an edge can report. Distinct codes
// let a plan branch without parsing text; the text first line is for people
// and grep.
const (
	edgeExitDisabled = 20
	edgeExitBlocked  = 21
)

// Disabled reasons. Kept to a small shared set so the same boundary reads
// the same way everywhere it is printed.
const (
	edgeReasonPlacement = "placement and instance lifecycle are decided on the hub"
	edgeReasonInventory = "host inventory is administered on the hub"
	edgeReasonDaemon    = "the daemon runs on the hub"
	edgeReasonLocalDB   = "local state administration has no meaning on an edge"
	edgeReasonSecret    = "local secrets never reach a job the hub runs"
	edgeReasonResults   = "an edge's credentials have no access to the results store"
	edgeReasonDisplay   = "interactive displays read the local ledger directly, which only the hub holds"
)

// Blocked causes for mirror and submit commands in this build. These name
// the missing piece of this binary, never a contacted hub: the edge does not
// connect to the hub, so reachability is a property of what the hub last
// published, and nothing here publishes or reads yet.
const (
	edgeCauseNoViewReader = "this build has no hub view reader yet"
	edgeCauseNoSubmitPath = "this build has no submission path yet"
)

type edgeModeEntry struct {
	mode   edgeMode
	reason string // required when mode is edgeModeDisabled
}

// edgeCommandModes assigns every runnable command (by full path, minus the
// "weft" root) its edge mode. Alias commands are keyed by their canonical
// path; cobra resolves invocation aliases to the same *cobra.Command.
var edgeCommandModes = map[string]edgeModeEntry{
	// --- local: no ledger, runs anywhere ---
	"aliases":               {edgeModeLocal, ""},
	"artifact":              {edgeModeLocal, ""}, // bare form prints help
	"build-agents":          {edgeModeLocal, ""},
	"bug tracker":           {edgeModeLocal, ""},
	"completion bash":       {edgeModeLocal, ""},
	"completion fish":       {edgeModeLocal, ""},
	"completion powershell": {edgeModeLocal, ""},
	"completion zsh":        {edgeModeLocal, ""},
	"edge doctor":           {edgeModeLocal, ""},
	"edge key add":          {edgeModeLocal, ""},
	"edge key list":         {edgeModeLocal, ""},
	"edge key mint":         {edgeModeLocal, ""},
	"edge key renew":        {edgeModeLocal, ""},
	"edge key revoke":       {edgeModeLocal, ""},
	"edge wait":             {edgeModeLocal, ""},
	"help":                  {edgeModeLocal, ""},
	"list aliases":          {edgeModeLocal, ""},
	"plan show":             {edgeModeLocal, ""},
	"plan validate":         {edgeModeLocal, ""},
	"sync inspect":          {edgeModeLocal, ""},
	"version":               {edgeModeLocal, ""},

	// --- mirror: render the hub's ledger from the hub view ---
	"artifact cat":         {edgeModeMirror, ""},
	"artifact get":         {edgeModeMirror, ""},
	"artifact list":        {edgeModeMirror, ""},
	"autopilot blocked":    {edgeModeMirror, ""},
	"autopilot status":     {edgeModeMirror, ""},
	"blackboard status":    {edgeModeMirror, ""},
	"bug list":             {edgeModeMirror, ""},
	"bug show":             {edgeModeMirror, ""},
	"cloud price-spread":   {edgeModeMirror, ""},
	"campaign list":        {edgeModeMirror, ""},
	"campaign show":        {edgeModeMirror, ""},
	"campaign cost":        {edgeModeMirror, ""},
	"campaign diagnose":    {edgeModeMirror, ""},
	"campaign stats":       {edgeModeMirror, ""},
	"campaign survival":    {edgeModeMirror, ""},
	"cost campaigns":       {edgeModeMirror, ""},
	"cost instances":       {edgeModeMirror, ""},
	"cost jobs":            {edgeModeMirror, ""},
	"data requests":        {edgeModeMirror, ""},
	"data where":           {edgeModeMirror, ""},
	"diagnose":             {edgeModeMirror, ""},
	"diagnose instance":    {edgeModeMirror, ""},
	"diagnose job":         {edgeModeMirror, ""},
	"estimation eval":      {edgeModeMirror, ""},
	"estimation status":    {edgeModeMirror, ""},
	"export training-data": {edgeModeMirror, ""},
	"host info":            {edgeModeMirror, ""},
	"host list":            {edgeModeMirror, ""},
	"incidents":            {edgeModeMirror, ""},
	"info":                 {edgeModeMirror, ""},
	"instance diagnose":    {edgeModeMirror, ""}, // same handler as diagnose instance
	"instance info":        {edgeModeMirror, ""}, // same handler as instance status
	"instance list":        {edgeModeMirror, ""},
	"instance status":      {edgeModeMirror, ""},
	"instance cost":        {edgeModeMirror, ""},
	"job anomalies":        {edgeModeMirror, ""},
	"job churn":            {edgeModeMirror, ""},
	"job cost":             {edgeModeMirror, ""},
	"job diagnose":         {edgeModeMirror, ""},
	"job diff":             {edgeModeMirror, ""},
	"job disk-calibration": {edgeModeMirror, ""},
	"job info":             {edgeModeMirror, ""},
	"job inspect":          {edgeModeMirror, ""},
	"job list":             {edgeModeMirror, ""},
	"job log":              {edgeModeMirror, ""},
	"job predict":          {edgeModeMirror, ""},
	"job recommend":        {edgeModeMirror, ""},
	"job status":           {edgeModeMirror, ""},
	"job telemetry":        {edgeModeMirror, ""},
	"job timeseries":       {edgeModeMirror, ""},
	"list":                 {edgeModeMirror, ""},
	"list artifacts":       {edgeModeMirror, ""},
	"list campaigns":       {edgeModeMirror, ""},
	"list hosts":           {edgeModeMirror, ""},
	"list instances":       {edgeModeMirror, ""},
	"list jobs":            {edgeModeMirror, ""},
	"list projects":        {edgeModeMirror, ""},
	"list queues":          {edgeModeMirror, ""},
	"queue list":           {edgeModeMirror, ""},
	"log":                  {edgeModeMirror, ""},
	"project":              {edgeModeMirror, ""},
	"project jobs":         {edgeModeMirror, ""},
	"project list":         {edgeModeMirror, ""},
	"project spent":        {edgeModeMirror, ""},
	"session unprocessed":  {edgeModeMirror, ""},
	"show":                 {edgeModeMirror, ""},
	"source cat":           {edgeModeMirror, ""},
	"source diff":          {edgeModeMirror, ""},
	"source inspect":       {edgeModeMirror, ""},
	"source ls":            {edgeModeMirror, ""},
	"status":               {edgeModeMirror, ""},
	"telemetry":            {edgeModeMirror, ""},

	// --- submit: the hub executes these on the edge's behalf ---
	"bug close":            {edgeModeSubmit, ""},
	"bug note":             {edgeModeSubmit, ""},
	"bug reopen":           {edgeModeSubmit, ""},
	"bug report":           {edgeModeSubmit, ""},
	"cancel":               {edgeModeSubmit, ""},
	"data add":             {edgeModeSubmit, ""},
	"data publish":         {edgeModeSubmit, ""},
	"edit":                 {edgeModeSubmit, ""},
	"job cancel":           {edgeModeSubmit, ""},
	"job describe":         {edgeModeSubmit, ""},
	"job draft":            {edgeModeSubmit, ""},
	"job kill":             {edgeModeSubmit, ""},
	"job mark-processed":   {edgeModeSubmit, ""},
	"job mark-unprocessed": {edgeModeSubmit, ""},
	"job pause":            {edgeModeSubmit, ""},
	"job priority":         {edgeModeSubmit, ""},
	"job restart":          {edgeModeSubmit, ""},
	"job resume":           {edgeModeSubmit, ""},
	"job run":              {edgeModeSubmit, ""},
	"job start":            {edgeModeSubmit, ""},
	"job tag add":          {edgeModeSubmit, ""},
	"job tag rm":           {edgeModeSubmit, ""},
	"job unpause":          {edgeModeSubmit, ""},
	"kill":                 {edgeModeSubmit, ""},
	"launch project":       {edgeModeSubmit, ""},
	"mark-processed":       {edgeModeSubmit, ""},
	"mark-unprocessed":     {edgeModeSubmit, ""},
	"pause":                {edgeModeSubmit, ""},
	"pause job":            {edgeModeSubmit, ""},
	"plan submit":          {edgeModeSubmit, ""},
	"project launch":       {edgeModeSubmit, ""},
	"queue edit":           {edgeModeSubmit, ""}, // alias of edit
	"restart":              {edgeModeSubmit, ""},
	"resume":               {edgeModeSubmit, ""},
	"run":                  {edgeModeSubmit, ""},
	"start":                {edgeModeSubmit, ""},
	"start project":        {edgeModeSubmit, ""},
	"tag add":              {edgeModeSubmit, ""},
	"tag rm":               {edgeModeSubmit, ""},
	"unpause":              {edgeModeSubmit, ""},
	"unpause job":          {edgeModeSubmit, ""},

	// --- disabled: placement and instance lifecycle ---
	"autopilot budget reset":          {edgeModeDisabled, edgeReasonPlacement},
	"autopilot pause":                 {edgeModeDisabled, edgeReasonPlacement},
	"autopilot resume":                {edgeModeDisabled, edgeReasonPlacement},
	"autopilot run":                   {edgeModeDisabled, edgeReasonPlacement},
	"budget":                          {edgeModeDisabled, edgeReasonPlacement},
	"campaign launch":                 {edgeModeDisabled, edgeReasonPlacement},
	"campaign safety resume":          {edgeModeDisabled, edgeReasonPlacement},
	"campaign terminate":              {edgeModeDisabled, edgeReasonPlacement},
	"cordon":                          {edgeModeDisabled, edgeReasonPlacement},
	"instance audit":                  {edgeModeDisabled, edgeReasonPlacement},
	"instance cordon":                 {edgeModeDisabled, edgeReasonPlacement},
	"instance disk-report":            {edgeModeDisabled, edgeReasonPlacement},
	"instance extend":                 {edgeModeDisabled, edgeReasonPlacement},
	"instance launch":                 {edgeModeDisabled, edgeReasonPlacement},
	"instance mark-credit-exhausted":  {edgeModeDisabled, edgeReasonPlacement},
	"instance mark-weft-bug":          {edgeModeDisabled, edgeReasonPlacement},
	"instance new":                    {edgeModeDisabled, edgeReasonPlacement},
	"instance release":                {edgeModeDisabled, edgeReasonPlacement},
	"instance ssh":                    {edgeModeDisabled, edgeReasonPlacement},
	"instance submit":                 {edgeModeDisabled, edgeReasonPlacement},
	"instance terminate":              {edgeModeDisabled, edgeReasonPlacement},
	"instance uncordon":               {edgeModeDisabled, edgeReasonPlacement},
	"job authorize-price":             {edgeModeDisabled, edgeReasonPlacement},
	"job move":                        {edgeModeDisabled, edgeReasonPlacement},
	"job place":                       {edgeModeDisabled, edgeReasonPlacement},
	"job unplace":                     {edgeModeDisabled, edgeReasonPlacement},
	"launch campaign":                 {edgeModeDisabled, edgeReasonPlacement},
	"launch instance":                 {edgeModeDisabled, edgeReasonPlacement},
	"move":                            {edgeModeDisabled, edgeReasonPlacement},
	"move jobs":                       {edgeModeDisabled, edgeReasonPlacement},
	"new instance":                    {edgeModeDisabled, edgeReasonPlacement},
	"place":                           {edgeModeDisabled, edgeReasonPlacement},
	"provider constraints":            {edgeModeDisabled, edgeReasonPlacement},
	"provider diagnose-startup":       {edgeModeDisabled, edgeReasonPlacement},
	"provider disable":                {edgeModeDisabled, edgeReasonPlacement},
	"provider enable":                 {edgeModeDisabled, edgeReasonPlacement},
	"provider list":                   {edgeModeDisabled, edgeReasonPlacement},
	"provider offers":                 {edgeModeDisabled, edgeReasonPlacement},
	"provider reset":                  {edgeModeDisabled, edgeReasonPlacement},
	"queue add":                       {edgeModeDisabled, edgeReasonInventory},
	"queue front":                     {edgeModeDisabled, edgeReasonInventory},
	"queue remove":                    {edgeModeDisabled, edgeReasonInventory},
	"queue start":                     {edgeModeDisabled, edgeReasonInventory},
	"queue status":                    {edgeModeDisabled, edgeReasonInventory},
	"queue stop":                      {edgeModeDisabled, edgeReasonInventory},
	"queue update":                    {edgeModeDisabled, edgeReasonInventory},
	"rebalance":                       {edgeModeDisabled, edgeReasonPlacement},
	"replan":                          {edgeModeDisabled, edgeReasonPlacement},
	"runpod backfill-metadata":        {edgeModeDisabled, edgeReasonPlacement},
	"runpod doctor":                   {edgeModeDisabled, edgeReasonPlacement},
	"runpod setup":                    {edgeModeDisabled, edgeReasonPlacement},
	"runpod template print-bootstrap": {edgeModeDisabled, edgeReasonPlacement},
	"sky import":                      {edgeModeDisabled, edgeReasonPlacement},
	"sky submit":                      {edgeModeDisabled, edgeReasonPlacement},
	"sky sync":                        {edgeModeDisabled, edgeReasonPlacement},
	"start campaign":                  {edgeModeDisabled, edgeReasonPlacement},
	"start instance":                  {edgeModeDisabled, edgeReasonPlacement},
	"terminate":                       {edgeModeDisabled, edgeReasonPlacement},
	"terminate campaign":              {edgeModeDisabled, edgeReasonPlacement},
	"terminate instance":              {edgeModeDisabled, edgeReasonPlacement},
	"uncordon":                        {edgeModeDisabled, edgeReasonPlacement},

	// --- disabled: host inventory ---
	"host capability observe": {edgeModeDisabled, edgeReasonInventory},
	"host data":               {edgeModeDisabled, edgeReasonInventory},
	"host discover":           {edgeModeDisabled, edgeReasonInventory},
	"host doctor":             {edgeModeDisabled, edgeReasonInventory},
	"host jobs":               {edgeModeDisabled, edgeReasonInventory},
	"host load":               {edgeModeDisabled, edgeReasonInventory},
	"host setup":              {edgeModeDisabled, edgeReasonInventory},

	// --- disabled: the daemon and its notification pipeline ---
	"channel install":  {edgeModeDisabled, edgeReasonDaemon},
	"channel serve":    {edgeModeDisabled, edgeReasonDaemon},
	"daemon install":   {edgeModeDisabled, edgeReasonDaemon},
	"daemon logs":      {edgeModeDisabled, edgeReasonDaemon},
	"daemon restart":   {edgeModeDisabled, edgeReasonDaemon},
	"daemon run":       {edgeModeDisabled, edgeReasonDaemon},
	"daemon start":     {edgeModeDisabled, edgeReasonDaemon},
	"daemon status":    {edgeModeDisabled, edgeReasonDaemon},
	"daemon stop":      {edgeModeDisabled, edgeReasonDaemon},
	"daemon uninstall": {edgeModeDisabled, edgeReasonDaemon},
	"slack test":       {edgeModeDisabled, edgeReasonDaemon},

	// --- disabled: local state administration ---
	"artifact add":                  {edgeModeDisabled, edgeReasonLocalDB},
	"artifact prune":                {edgeModeDisabled, edgeReasonLocalDB},
	"artifact sync":                 {edgeModeDisabled, edgeReasonLocalDB},
	"cleanup":                       {edgeModeDisabled, edgeReasonLocalDB},
	"data evict":                    {edgeModeDisabled, edgeReasonLocalDB},
	"data fetch":                    {edgeModeDisabled, edgeReasonLocalDB},
	"db archive-telemetry":          {edgeModeDisabled, edgeReasonLocalDB},
	"db gc":                         {edgeModeDisabled, edgeReasonLocalDB},
	"db snapshot":                   {edgeModeDisabled, edgeReasonLocalDB},
	"estimation train":              {edgeModeDisabled, edgeReasonLocalDB},
	"job cleanup":                   {edgeModeDisabled, edgeReasonLocalDB},
	"job repair cloud-completions":  {edgeModeDisabled, edgeReasonLocalDB},
	"job repair duplicate-attempts": {edgeModeDisabled, edgeReasonLocalDB},
	"job repair sync-state":         {edgeModeDisabled, edgeReasonLocalDB},
	"retrain":                       {edgeModeDisabled, edgeReasonLocalDB},
	"sync":                          {edgeModeDisabled, edgeReasonLocalDB},

	// --- disabled: local secrets ---
	"secret list":   {edgeModeDisabled, edgeReasonSecret},
	"secret remove": {edgeModeDisabled, edgeReasonSecret},
	"secret set":    {edgeModeDisabled, edgeReasonSecret},

	// --- disabled: raw results-store access ---
	"r2 cat":          {edgeModeDisabled, edgeReasonResults},
	"r2 content-info": {edgeModeDisabled, edgeReasonResults},
	"r2 ls":           {edgeModeDisabled, edgeReasonResults},
	"r2 put-content":  {edgeModeDisabled, edgeReasonResults},
	"r2 status":       {edgeModeDisabled, edgeReasonResults},

	// --- disabled: interactive display layers ---
	"campaign watch": {edgeModeDisabled, edgeReasonDisplay},
	"cloud watch":    {edgeModeDisabled, edgeReasonDisplay},
	"dashboard":      {edgeModeDisabled, edgeReasonDisplay},
	"instance watch": {edgeModeDisabled, edgeReasonDisplay},
	"job watch":      {edgeModeDisabled, edgeReasonDisplay},
	"narrate":        {edgeModeDisabled, edgeReasonDisplay},
	"project watch":  {edgeModeDisabled, edgeReasonDisplay},
	"system watch":   {edgeModeDisabled, edgeReasonDisplay},
	"tui":            {edgeModeDisabled, edgeReasonDisplay},
	"watch":          {edgeModeDisabled, edgeReasonDisplay},
	"watch campaign": {edgeModeDisabled, edgeReasonDisplay},
	"watch instance": {edgeModeDisabled, edgeReasonDisplay},
	"watch jobs":     {edgeModeDisabled, edgeReasonDisplay},
	"watch project":  {edgeModeDisabled, edgeReasonDisplay},
	"watch system":   {edgeModeDisabled, edgeReasonDisplay},
	"web":            {edgeModeDisabled, edgeReasonDisplay},
}

// EdgeGateError is returned by the edge-mode gate for a command the edge
// refused to run. The gate has already printed the user-facing line (text on
// stderr, or the JSON shape on stdout); Execute must not print it again, and
// main maps the error to ExitCode. ViewAgeS carries the hub view's age in
// seconds when a blocked outcome knows it (the stale-section case); it is nil
// whenever no publication time was available to measure.
type EdgeGateError struct {
	Outcome  string // "disabled" or "blocked"
	Detail   string // the reason or cause, as printed
	ExitCode int
	ViewAgeS *float64
}

func (e *EdgeGateError) Error() string {
	switch e.Outcome {
	case "disabled":
		return "disabled on an edge: " + e.Detail
	default:
		return "hub not reachable from this edge: " + e.Detail
	}
}

// edgeGateJSON is the machine-readable blocked-point shape: the only place
// the word "edge" appears in ordinary command output.
type edgeGateJSON struct {
	Edge struct {
		Outcome  string   `json:"outcome"`
		Reason   string   `json:"reason,omitempty"`
		Cause    string   `json:"cause,omitempty"`
		ViewAgeS *float64 `json:"view_age_s,omitempty"`
	} `json:"edge"`
}

// applyEdgeGate enforces the edge-role command classification for one
// invocation. args are the post-alias-rewrite command arguments (no program
// name). It returns nil when the command may run: in the hub role, for local
// commands, for help output, and for anything cobra itself will reject. A
// non-nil return is an *EdgeGateError with its output already written.
func applyEdgeGate(cfg *config.Config, args []string) error {
	if cfg == nil || !cfg.Edge.IsEdge() {
		return nil
	}
	annotateEdgeHelp()

	// Resolve against the same tree ExecuteC will use, including the help and
	// completion commands cobra installs lazily.
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd()
	resolved, _, err := rootCmd.Find(args)
	if err != nil || resolved == nil {
		// Not a resolvable invocation; cobra's own error path will report it.
		return nil
	}
	if resolved == rootCmd || !resolved.Runnable() || edgeHelpRequested(args) {
		return nil
	}
	path := edgeCommandPath(resolved)
	// The ledger backstop covers every command, including local ones: a code
	// path that reaches for a database the table allowed past the gate still
	// fails loudly instead of manufacturing an empty ledger. The refusal
	// lasts for the process lifetime; the restore function exists for tests.
	db.RefuseLocalLedger(path)

	entry, ok := edgeCommandModes[path]
	if !ok {
		// The tree-walk test makes this unreachable for a command in this
		// binary; a command that somehow slips through is blocked, never run.
		return edgeBlock(resolved, args, &EdgeGateError{
			Outcome:  "blocked",
			Detail:   fmt.Sprintf("no edge-mode classification for %q (this is a bug in weft)", path),
			ExitCode: edgeExitBlocked,
		})
	}
	switch entry.mode {
	case edgeModeLocal:
		return nil
	case edgeModeDisabled:
		return edgeBlock(resolved, args, &EdgeGateError{
			Outcome:  "disabled",
			Detail:   entry.reason,
			ExitCode: edgeExitDisabled,
		})
	case edgeModeMirror:
		if _, served := edgeMirrorServed[path]; !served {
			// A mirror command with no view section yet blocks honestly rather
			// than falling through to the ledger.
			return edgeBlock(resolved, args, &EdgeGateError{
				Outcome:  "blocked",
				Detail:   edgeCauseNoViewReader,
				ExitCode: edgeExitBlocked,
			})
		}
		runtime, err := newEdgeMirrorRuntime(cfg, edgeAllowStale, args)
		if err != nil {
			return edgeBlock(resolved, args, &EdgeGateError{
				Outcome:  "blocked",
				Detail:   err.Error(),
				ExitCode: edgeExitBlocked,
			})
		}
		activeEdgeMirror = runtime
		return nil
	case edgeModeSubmit:
		return edgeBlock(resolved, args, &EdgeGateError{
			Outcome:  "blocked",
			Detail:   edgeCauseNoSubmitPath,
			ExitCode: edgeExitBlocked,
		})
	}
	return nil
}

// edgeBlock emits the blocked point — JSON on stdout when the command
// declares --json and it was passed, the text line on stderr otherwise — and
// returns the error for the exit-status mapping.
func edgeBlock(cmd *cobra.Command, args []string, gateErr *EdgeGateError) error {
	if edgeJSONRequested(cmd, args) {
		var shape edgeGateJSON
		shape.Edge.Outcome = gateErr.Outcome
		if gateErr.Outcome == "disabled" {
			shape.Edge.Reason = gateErr.Detail
		} else {
			shape.Edge.Cause = gateErr.Detail
			shape.Edge.ViewAgeS = gateErr.ViewAgeS
		}
		data, err := json.Marshal(shape)
		if err != nil {
			return fmt.Errorf("encode edge gate result: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return gateErr
	}
	fmt.Fprintln(cmd.ErrOrStderr(), gateErr.Error())
	return gateErr
}

// edgeCommandPath is the table key for a command: its cobra path minus the
// root name.
func edgeCommandPath(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), rootCmd.Name()+" ")
}

// edgeHelpRequested reports whether the invocation asks for help, which must
// work for every command on an edge.
func edgeHelpRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// edgeJSONRequested reports whether the invocation asks for machine-readable
// output: --json on a command that declares it, or --format json on a command
// that declares a format flag (`weft list` and `weft jobs list` take the
// latter, and that is the surface agents actually use).
func edgeJSONRequested(cmd *cobra.Command, args []string) bool {
	jsonFlag := cmd.Flags().Lookup("json") != nil
	formatFlag := cmd.Flags().Lookup("format") != nil
	for _, a := range args {
		if a == "--" {
			return false
		}
		if jsonFlag {
			if a == "--json" {
				return true
			}
			if rest, ok := strings.CutPrefix(a, "--json="); ok {
				return rest == "true"
			}
		}
		if formatFlag {
			if a == "--format" {
				// The value is the next argument; look for it in the rest of
				// the scan rather than ending the search here.
				continue
			}
			if rest, ok := strings.CutPrefix(a, "--format="); ok {
				return rest == "json"
			}
		}
	}
	if formatFlag {
		for i, a := range args {
			if a == "--" {
				return false
			}
			if a == "--format" && i+1 < len(args) {
				return args[i+1] == "json"
			}
		}
	}
	return false
}

var edgeHelpAnnotated bool

// annotateEdgeHelp marks disabled commands in help output so an agent
// reading --help on an edge sees the boundary without opening the docs.
func annotateEdgeHelp() {
	if edgeHelpAnnotated {
		return
	}
	edgeHelpAnnotated = true
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if entry, ok := edgeCommandModes[edgeCommandPath(c)]; ok && entry.mode == edgeModeDisabled {
			c.Short = "[disabled on an edge] " + c.Short
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}

// isEdgeGateError reports whether err came from the edge-mode gate, whose
// output is already written.
func isEdgeGateError(err error) bool {
	var ge *EdgeGateError
	return errors.As(err, &ge)
}
