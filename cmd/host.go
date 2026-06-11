package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/osteele/weft/internal/util"
	"github.com/spf13/cobra"
)

const (
	hostInfoLiveProbeTimeout = 2 * time.Second
	hostDataScanTimeout      = 10 * time.Minute
)

var hostCmd = &cobra.Command{
	Use:     "host",
	Aliases: []string{"hosts"},
	Short:   "Show information about remote hosts",
	Long: `Show information about remote hosts including system info, active jobs, and load.

Available subcommands:
  info      Show system information (CPU, memory, GPUs)
  jobs      List active jobs on host
  load      Show current load and resource usage`,
}

var hostInfoCmd = &cobra.Command{
	Use:   "info <host>",
	Short: "Show system information for a host",
	Long: `Show system information including CPU, memory, and GPU details.

Example:
  weft host info cool30`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runHostInfo,
}

var hostJobsCmd = &cobra.Command{
	Use:   "jobs <host>",
	Short: "List active jobs on a host",
	Long: `List all active (running and queued) jobs on the specified host.

Example:
  weft host jobs cool30`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runHostJobs,
}

var hostDataCmd = &cobra.Command{
	Use:   "data [host]",
	Short: "Show data assets on hosts",
	Long: `Show HuggingFace models, datasets, and other data assets known to exist on hosts.

Without a host argument, shows a cross-host map of all known assets.
With a host argument, shows assets on that specific host.

Use --scan with a host to scan the remote host's HF cache and update the local database.

Examples:
  weft host data                  # Show cross-host asset map
  weft host data cool100          # Show cached data inventory for cool100
  weft host data cool100 --scan   # Scan remote HF cache and update`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
	RunE: runHostData,
}

var hostDataScan bool

var hostListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all known hosts and their capabilities",
	Long: `List on-prem inventory hosts and currently-running cloud rental instances,
with OS, architecture, and GPU specs.

Examples:
  weft host list
  weft host list --rentals=only
  weft host list --rentals=off`,
	Args: cobra.NoArgs,
	RunE: runHostList,
}

// hostListRentalsFlag controls whether cloud rentals appear in `host list`.
// Accepted values: "on" (default), "off", "only".
var hostListRentalsFlag = "on"

// hostListTUIFlag, when set, opens an interactive hosts panel instead of
// printing the text table. Routes through the same watch-router used by
// `weft uj` so the same key bindings (J jobs, i instances, q quit) work.
var hostListTUIFlag bool

var hostLoadCmd = &cobra.Command{
	Use:   "load <host>",
	Short: "Show current load and resource usage",
	Long: `Show current CPU, memory, and GPU usage for a host.

Example:
  weft host load cool30`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runHostLoad,
}

var hostDiscoverCmd = &cobra.Command{
	Use:   "discover <hostname>",
	Short: "Probe a host via SSH and generate a host inventory file",
	Long: `SSH into the specified host, probe its hardware (CPU, memory, GPUs),
and write a YAML inventory file to ~/.config/weft/hosts/<hostname>.yaml.

Example:
  weft host discover cool30`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runHostDiscover,
}

func init() {
	rootCmd.AddCommand(hostCmd)
	hostCmd.AddCommand(hostDataCmd)
	hostCmd.AddCommand(hostInfoCmd)
	hostCmd.AddCommand(hostJobsCmd)
	hostCmd.AddCommand(hostListCmd)
	hostCmd.AddCommand(hostLoadCmd)
	hostCmd.AddCommand(hostDiscoverCmd)
	hostCmd.AddCommand(hostDoctorCmd)
	hostCmd.AddCommand(hostSetupCmd)

	hostDataCmd.Flags().BoolVar(&hostDataScan, "scan", false, "Scan remote HF cache and update local database")
	hostListCmd.Flags().StringVar(&hostListRentalsFlag, "rentals", "on",
		"Whether to include cloud rentals in the list: on (default), off, only")
	hostListCmd.Flags().BoolVar(&hostListTUIFlag, "tui", false,
		"Open an interactive hosts panel with live CPU/MEM/GPU bars")
}

func runHostInfo(cmd *cobra.Command, args []string) error {
	host := args[0]

	// Cloud instances are managed via `weft instance status`; accept both
	// `rental:NN` (legacy) and the canonical `wiNNN` form here and delegate.
	if instanceID, ok := strings.CutPrefix(host, "rental:"); ok {
		return runInstanceStatus(cmd, []string{instanceID})
	}
	if isInstanceIDArg(host) {
		return runInstanceStatus(cmd, []string{host})
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Try to get cached host info first
	cachedInfo, err := db.LoadCachedHostInfo(database, host)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("load cached info: %w", err)
	}

	// Best-effort live probe for dynamic utilization data.
	// Fail fast and quietly when the host is unavailable.
	var liveHost *hostinfo.Host
	if _, probedHost, probeErr := ops.TryFetchAndCacheHostInfo(database, host, hostInfoLiveProbeTimeout); probeErr == nil && probedHost != nil {
		liveHost = probedHost
		if cachedInfo == nil {
			cachedInfo = hostinfo.CachedInfoFromHost(probedHost)
		}
	}

	// Display cached info if available
	if cachedInfo != nil {
		displayHostInfo(host, cachedInfo, liveHost)
		cacheAge := time.Now().Unix() - cachedInfo.LastUpdated
		fmt.Printf("\n(cached %s ago)\n", db.FormatDuration(cacheAge))
	} else {
		fmt.Printf("No cached information for %s\n", host)
		fmt.Printf("Run 'weft host discover %s' to probe and cache host information\n", host)
	}

	return nil
}

func displayHostInfo(host string, info *db.CachedHostInfo, liveHost *hostinfo.Host) {
	cachedHost := hostinfo.HostFromCachedInfo(info)
	if cachedHost == nil {
		cachedHost = &hostinfo.Host{}
	}

	fmt.Printf("Host: %s\n", host)
	if cachedHost.Arch != "" {
		fmt.Printf("Architecture: %s\n", cachedHost.Arch)
	}
	if cachedHost.Model != "" {
		fmt.Printf("Model: %s\n", cachedHost.Model)
	}
	if cachedHost.OS != "" {
		fmt.Printf("OS: %s\n", cachedHost.OS)
	}
	if cachedHost.CPUs > 0 {
		fmt.Printf("CPUs: %d", cachedHost.CPUs)
		if cachedHost.CPUModel != "" {
			fmt.Printf(" (%s", cachedHost.CPUModel)
			if cachedHost.CPUFreq != "" {
				fmt.Printf(" @ %s", cachedHost.CPUFreq)
			}
			fmt.Printf(")")
		}
		fmt.Println()
	}
	if cachedHost.MemTotal != "" {
		fmt.Printf("Memory: %s\n", cachedHost.MemTotal)
	}
	if liveHost != nil {
		if pct := liveHost.CPUUtilizationPct(); pct >= 0 {
			load := strings.TrimSpace(liveHost.LoadAvgShort())
			if load != "" && load != "-" && liveHost.CPUs > 0 {
				fmt.Printf("CPU Utilization: %d%% (load1 %s on %d cores)\n", pct, load, liveHost.CPUs)
			} else {
				fmt.Printf("CPU Utilization: %d%%\n", pct)
			}
		}
	}

	if len(cachedHost.GPUs) > 0 {
		displayHostGPUStats(cachedHost.GPUs)
		return
	}

	gpusJSON := strings.TrimSpace(info.GPUsJSON)
	if gpusJSON != "" && gpusJSON != "[]" {
		fmt.Printf("\nGPUs: %s\n", gpusJSON)
	}
}

func displayHostGPUStats(gpus []hostinfo.GPUInfo) {
	sorted := append([]hostinfo.GPUInfo(nil), gpus...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Index < sorted[j].Index
	})

	fmt.Printf("\nGPUs:\n")
	const rowFmt = "  %-3s %-6s %-6s %-22s %s\n"
	fmt.Printf(rowFmt, "GPU", "Temp", "Util", "Memory", "Name")
	fmt.Printf(rowFmt, "───", "────", "────", "──────────────────────", "────")
	for _, gpu := range sorted {
		temp := "-"
		if gpu.Temperature > 0 {
			temp = fmt.Sprintf("%d°C", gpu.Temperature)
		}
		util := "-"
		if gpu.Utilization > 0 || gpu.MemUsed != "" || gpu.MemTotal != "" {
			util = fmt.Sprintf("%d%%", gpu.Utilization)
		}
		mem := formatGPUMemoryUsage(gpu.MemUsed, gpu.MemTotal)
		name := strings.TrimSpace(gpu.Name)
		if name == "" {
			name = "(unknown)"
		}
		fmt.Printf(rowFmt, strconv.Itoa(gpu.Index), temp, util, mem, name)
	}
}

func formatGPUMemoryUsage(used, total string) string {
	used = strings.TrimSpace(used)
	total = strings.TrimSpace(total)
	if used == "" && total == "" {
		return "-"
	}
	if used == "" {
		return formatGPUMemory(total)
	}
	if total == "" {
		return formatGPUMemory(used)
	}
	usedFmt := formatGPUMemory(used)
	totalFmt := formatGPUMemory(total)
	usedMiB, usedOK := parseMemMiB(used)
	totalMiB, totalOK := parseMemMiB(total)
	if usedOK && totalOK && totalMiB > 0 {
		pct := (usedMiB * 100) / totalMiB
		return fmt.Sprintf("%s/%s(%d%%)", usedFmt, totalFmt, pct)
	}
	return fmt.Sprintf("%s/%s", usedFmt, totalFmt)
}

func formatGPUMemory(mem string) string {
	mem = strings.TrimSpace(mem)
	mib, ok := parseMemMiB(mem)
	if !ok {
		return mem
	}
	if mib >= 1024 {
		return fmt.Sprintf("%.1fGiB", float64(mib)/1024.0)
	}
	return fmt.Sprintf("%dMiB", mib)
}

func parseMemMiB(mem string) (int, bool) {
	mem = strings.ReplaceAll(strings.TrimSpace(mem), " ", "")
	if mem == "" {
		return 0, false
	}
	i := 0
	for i < len(mem) && (mem[i] == '.' || (mem[i] >= '0' && mem[i] <= '9')) {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(mem[:i], 64)
	if err != nil {
		return 0, false
	}
	unit := strings.ToLower(mem[i:])
	switch unit {
	case "", "m", "mb", "mib":
		return int(n + 0.5), true
	case "g", "gb", "gib":
		return int(n*1024.0 + 0.5), true
	default:
		return 0, false
	}
}

func runHostJobs(cmd *cobra.Command, args []string) error {
	host := args[0]

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Cloud instance: list jobs associated with the launch instead of jobs
	// keyed by hostname (cloud jobs don't carry a real host string).
	var jobs []*db.Job
	if isInstanceIDArg(host) {
		id, perr := ids.ParseInstanceID(host)
		if perr != nil {
			return fmt.Errorf("parse instance ID %q: %w", host, perr)
		}
		jobs, err = db.GetLaunchJobs(database, id)
	} else {
		jobs, err = db.ListActiveJobs(database, host)
	}

	if len(jobs) == 0 {
		fmt.Printf("No active jobs on %s\n", host)
		return nil
	}

	// Display jobs in a table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tSTATUS\tSTARTED\tCOMMAND / DESCRIPTION\n")

	for _, job := range jobs {
		started := formatHostJobStarted(job.StartTime)

		display := job.Description
		if display == "" {
			display = job.EffectiveCommand()
		}
		if len(display) > 50 {
			display = display[:47] + "..."
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			ids.FormatJobID(job.ID), job.Status, started, display)
	}

	w.Flush()
	fmt.Printf("\nTotal: %d active job(s) on %s\n", len(jobs), host)

	return nil
}

func formatHostJobStarted(startTime int64) string {
	if startTime <= 0 {
		return "-"
	}
	return time.Unix(startTime, 0).Format("01/02 15:04")
}

func runHostLoad(cmd *cobra.Command, args []string) error {
	target := args[0]

	fmt.Printf("Fetching current load for %s...\n", target)

	run, label, err := loadTargetRunner(target)
	if err != nil {
		return err
	}

	// Get uptime and load average
	stdout, err := run("uptime")
	if err != nil {
		return fmt.Errorf("get uptime: %w", err)
	}

	fmt.Printf("\n%s\n", label)
	fmt.Printf("Uptime: %s\n", strings.TrimSpace(stdout))

	// Get memory info
	if stdout, err := run("free -h | grep Mem"); err == nil {
		parts := strings.Fields(stdout)
		if len(parts) >= 3 {
			fmt.Printf("\nMemory:\n")
			fmt.Printf("  Total: %s\n", parts[1])
			fmt.Printf("  Used: %s\n", parts[2])
			if len(parts) >= 4 {
				fmt.Printf("  Free: %s\n", parts[3])
			}
		}
	}

	// Get GPU info if nvidia-smi is available
	gpuCmd := "nvidia-smi --query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu --format=csv,noheader,nounits 2>/dev/null || echo ''"
	if stdout, err := run(gpuCmd); err == nil && strings.TrimSpace(stdout) != "" {
		fmt.Printf("\nGPUs:\n")
		for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
			parts := strings.Split(line, ", ")
			if len(parts) >= 6 {
				fmt.Printf("  GPU %s: %s\n", parts[0], parts[1])
				fmt.Printf("    Utilization: %s%%\n", parts[2])
				fmt.Printf("    Memory: %s MiB / %s MiB\n", parts[3], parts[4])
				fmt.Printf("    Temperature: %s°C\n", parts[5])
			}
		}
	}

	return nil
}

// isInstanceIDArg reports whether s looks like a CLI instance ID token (wiNNN).
// A bare numeric value is ambiguous (could be a hostname) and is treated as a
// host name.
func isInstanceIDArg(s string) bool {
	return len(s) > 2 && strings.EqualFold(s[:2], "wi") && s[2] >= '0' && s[2] <= '9'
}

// runOnTarget runs a shell command against an on-prem host or a cloud
// instance (when target is wiNNN). Callers receive only stdout; stderr is
// folded into the returned error on failure. Use loadTargetRunner when
// dispatching several commands in a row to avoid repeated DB/provider lookups.
func runOnTarget(target string, command string) (string, error) {
	run, _, err := loadTargetRunner(target)
	if err != nil {
		return "", err
	}
	return run(command)
}

// loadTargetRunner returns a function that runs a shell command on the named
// target, which may be an on-prem host (from the inventory) or a cloud
// instance ID (wiNNN). The returned label is the heading to display in CLI
// output (e.g. "Host: cool30" or "Instance: wi3122 (vastai 12345678)").
//
// For cloud instances the function holds a resolved *cloud.Instance, so
// subsequent calls reuse the same SSH coordinates without re-querying the
// provider.
func loadTargetRunner(target string) (func(string) (string, error), string, error) {
	if !isInstanceIDArg(target) {
		run := func(c string) (string, error) {
			stdout, _, err := ssh.Run(target, c)
			return stdout, err
		}
		return run, "Host: " + target, nil
	}

	id, err := ids.ParseInstanceID(target)
	if err != nil {
		return nil, "", fmt.Errorf("parse instance ID %q: %w", target, err)
	}
	database, err := db.OpenForReading()
	if err != nil {
		return nil, "", fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	ci, err := db.GetLaunch(database, id)
	if err != nil {
		return nil, "", fmt.Errorf("get instance: %w", err)
	}
	if ci == nil {
		return nil, "", fmt.Errorf("instance %s not found", ids.FormatInstanceID(id))
	}
	providerID := ci.EffectiveProviderID()
	if providerID == "" {
		return nil, "", fmt.Errorf("instance %s has no provider ID yet (status=%s)", ids.FormatInstanceID(id), ci.Status)
	}
	client := cloudClientForDBInstance(ci.Provider)
	inst, err := client.ShowInstance(providerID)
	if err != nil {
		return nil, "", fmt.Errorf("show instance: %w", err)
	}
	label := fmt.Sprintf("Instance: %s (%s %s)", ids.FormatInstanceID(id), ci.Provider, providerID)
	run := func(c string) (string, error) {
		return cloud.RunOnInstance(inst, c, 30*time.Second)
	}
	return run, label, nil
}

func runHostData(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Cross-host map when no host specified
	if len(args) == 0 {
		return runHostDataMap(database)
	}

	host := args[0]

	if isInstanceIDArg(host) {
		return fmt.Errorf("weft host data is for inventory hosts; cloud instance data is ephemeral. "+
			"To list HF/corpus assets on a cloud instance, SSH in with 'weft instance ssh %s' and run 'ls ~/.cache/huggingface/hub' or 'huggingface-cli scan-cache'", host)
	}

	if hostDataScan {
		fmt.Printf("Scanning HuggingFace cache on %s...\n", host)
		ctx, cancel := context.WithTimeout(context.Background(), hostDataScanTimeout)
		defer cancel()
		entries, err := dataloc.ScanHFCacheDetailedContext(ctx, host)
		if err != nil {
			return fmt.Errorf("scan HF cache: %w", err)
		}
		now := time.Now()
		for _, entry := range entries {
			entry.LastSeen = now
			if err := dataloc.RecordAsset(database, entry); err != nil {
				return fmt.Errorf("record asset: %w", err)
			}
		}
		fmt.Printf("Found %d HF asset(s)\n", len(entries))
		// The scan ran to completion (SSH or non-zero shell exit would have
		// errored above), so any stale HF entries can be pruned safely.
		if err := pruneStaleAssets(database, host, now, "HF",
			dataloc.AssetHFModel, dataloc.AssetHFDataset); err != nil {
			return err
		}

		fmt.Printf("Scanning corpus directory on %s...\n", host)
		corpusEntries, err := dataloc.ScanCorpusDir(host)
		if err != nil {
			return fmt.Errorf("scan corpus dir: %w", err)
		}
		for _, entry := range corpusEntries {
			entry.LastSeen = now
			if err := dataloc.RecordAsset(database, entry); err != nil {
				return fmt.Errorf("record asset: %w", err)
			}
		}
		if len(corpusEntries) > 0 {
			fmt.Printf("Found %d corpus asset(s)\n", len(corpusEntries))
		}
		if err := pruneStaleAssets(database, host, now, "corpus", dataloc.AssetCorpus); err != nil {
			return err
		}
	}

	entries, err := dataloc.ListHostAssets(database, host)
	if err != nil {
		return fmt.Errorf("list assets: %w", err)
	}

	if len(entries) == 0 {
		fmt.Printf("No data assets known on %s\n", host)
		if !hostDataScan {
			fmt.Printf("Run with --scan to discover HuggingFace models and datasets\n")
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "KIND\tID\tLAST SEEN\n")
	for _, e := range entries {
		age := time.Since(e.LastSeen)
		fmt.Fprintf(w, "%s\t%s\t%s ago\n", e.Asset.Kind, e.Asset.ID, db.FormatDuration(int64(age.Seconds())))
	}
	w.Flush()

	return nil
}

// pruneStaleAssets removes host_data entries on host whose kind is in kinds
// and whose last_seen is older than now, then prints a one-line summary.
// label appears in error messages and the summary ("HF", "corpus", ...).
func pruneStaleAssets(database *sql.DB, host string, now time.Time, label string, kinds ...dataloc.AssetKind) error {
	removed, err := dataloc.RemoveStaleEntriesForKinds(database, host, now, kinds)
	if err != nil {
		return fmt.Errorf("prune stale %s entries: %w", label, err)
	}
	if removed > 0 {
		noun := "entries"
		if removed == 1 {
			noun = "entry"
		}
		fmt.Printf("Removed %d stale %s %s no longer present on %s\n", removed, label, noun, host)
	}
	return nil
}

func runHostDataMap(database *sql.DB) error {
	entries, err := dataloc.ListAllAssets(database)
	if err != nil {
		return fmt.Errorf("list assets: %w", err)
	}

	if len(entries) == 0 {
		fmt.Println("No data assets known on any host")
		fmt.Println("Run 'weft host data <host> --scan' to discover assets, or sync will auto-scan")
		return nil
	}

	// Group by asset
	type assetKey struct {
		Kind dataloc.AssetKind
		ID   string
	}
	type assetInfo struct {
		key   assetKey
		hosts []string
	}

	seen := make(map[assetKey]*assetInfo)
	var order []assetKey
	for _, e := range entries {
		k := assetKey{e.Asset.Kind, e.Asset.ID}
		if info, ok := seen[k]; ok {
			info.hosts = append(info.hosts, e.Host)
		} else {
			seen[k] = &assetInfo{key: k, hosts: []string{e.Host}}
			order = append(order, k)
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "KIND\tID\tHOSTS\n")
	for _, k := range order {
		info := seen[k]
		fmt.Fprintf(w, "%s\t%s\t%s\n", k.Kind, k.ID, strings.Join(info.hosts, ", "))
	}
	w.Flush()

	return nil
}

func runHostList(cmd *cobra.Command, args []string) error {
	mode, err := parseHostListRentalsMode(hostListRentalsFlag)
	if err != nil {
		return err
	}

	if hostListTUIFlag {
		return runHostListTUI()
	}

	rows, err := loadHostListRows(time.Now(), mode)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "TYPE\tNAME\tOS/ARCH\tCPU\tMEMORY\tGPUs\n")
	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			row.Type, row.Name, row.OSArch, row.CPU, row.Memory, row.GPUs)
	}
	return w.Flush()
}

// runHostListTUI opens the interactive hosts panel.
func runHostListTUI() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	return terminal.RunHostsTUI(database, cfg)
}

type hostListRentalsMode int

const (
	rentalsOn hostListRentalsMode = iota
	rentalsOff
	rentalsOnly
)

func parseHostListRentalsMode(s string) (hostListRentalsMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "on":
		return rentalsOn, nil
	case "off":
		return rentalsOff, nil
	case "only":
		return rentalsOnly, nil
	default:
		return 0, fmt.Errorf("invalid --rentals value %q: want on, off, or only", s)
	}
}

type hostListRow struct {
	Type   string // "host" or "rental"
	Name   string
	OSArch string
	CPU    string
	Memory string
	GPUs   string
}

func loadHostListRows(now time.Time, mode hostListRentalsMode) ([]hostListRow, error) {
	hosts, err := inventory.LoadHosts()
	if err != nil {
		return nil, fmt.Errorf("load inventory: %w", err)
	}

	specByName := make(map[string]inventory.HostSpec, len(hosts))
	for _, host := range hosts {
		if host.Name == "" || db.IsLaunchHost(host.Name) {
			continue
		}
		specByName[host.Name] = host
	}

	cachedByName := map[string]*db.CachedHostInfo{}
	recentHosts := map[string]struct{}{}
	database, err := db.Open()
	if err == nil {
		defer database.Close()

		for _, name := range listRecentHostNames(database, now.Add(-defaultHostSyncWindow)) {
			if name != "" && !db.IsLaunchHost(name) {
				recentHosts[name] = struct{}{}
			}
		}

		cachedHosts, cacheErr := db.LoadAllCachedHosts(database)
		if cacheErr == nil {
			for _, cached := range cachedHosts {
				if cached == nil || cached.Name == "" || db.IsLaunchHost(cached.Name) {
					continue
				}
				cachedByName[cached.Name] = cached
				if cached.LastUpdated >= now.Add(-defaultHostSyncWindow).Unix() {
					recentHosts[cached.Name] = struct{}{}
				}
			}
		}

		activeJobs, activeErr := db.ListActiveOnPremJobs(database)
		if activeErr == nil {
			for _, job := range activeJobs {
				if job == nil || !job.HasInventoryHost() {
					continue
				}
				recentHosts[job.Host] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(specByName)+len(recentHosts))
	for name := range specByName {
		names = append(names, name)
	}
	for name := range recentHosts {
		if _, ok := specByName[name]; !ok {
			names = append(names, name)
		}
	}
	util.NaturalSortStrings(names)

	rows := make([]hostListRow, 0, len(names))
	if mode != rentalsOnly {
		for _, name := range names {
			if spec, ok := specByName[name]; ok {
				rows = append(rows, hostListRowFromSpec(spec))
				continue
			}
			if cached := cachedByName[name]; cached != nil {
				host := hostinfo.HostFromCachedInfo(cached)
				if host != nil {
					host.Name = name
					rows = append(rows, hostListRowFromSpec(inventory.HostSpecFromHostInfo(name, host, "")))
					continue
				}
			}
			rows = append(rows, hostListUnknownRow(name))
		}
	}

	if mode != rentalsOff && database != nil {
		launches, lerr := db.ListRunningLaunches(database)
		if lerr == nil {
			rentalRows := make([]hostListRow, 0, len(launches))
			for _, launch := range launches {
				if launch == nil || launch.ID <= 0 {
					continue
				}
				rentalRows = append(rentalRows, hostListRowFromLaunch(launch))
			}
			sort.SliceStable(rentalRows, func(i, j int) bool {
				return util.NaturalLess(rentalRows[i].Name, rentalRows[j].Name)
			})
			rows = append(rows, rentalRows...)
		}
	}

	return rows, nil
}

func hostListRowFromLaunch(launch *db.Launch) hostListRow {
	provider := strings.TrimSpace(launch.Provider)
	if provider == "" {
		provider = "rental"
	}
	gpus := strings.TrimSpace(launch.DisplayGPUSpec())
	if gpus == "" {
		gpus = "unknown"
	}
	return hostListRow{
		Type:   "rental",
		Name:   ids.FormatInstanceID(launch.ID),
		OSArch: provider,
		CPU:    "—",
		Memory: "—",
		GPUs:   gpus,
	}
}

func listRecentHostNames(database *sql.DB, since time.Time) []string {
	names, err := db.ListHostsSyncedSince(database, since)
	if err != nil {
		return nil
	}
	return names
}

func hostListRowFromSpec(spec inventory.HostSpec) hostListRow {
	osArch := "unknown/unknown"
	if spec.OS != "" || spec.Arch != "" {
		osPart := spec.OS
		if osPart == "" {
			osPart = "unknown"
		}
		archPart := spec.Arch
		if archPart == "" {
			archPart = "unknown"
		}
		osArch = osPart + "/" + archPart
	}

	cpu := "unknown"
	if spec.CPUCores > 0 {
		cpu = fmt.Sprintf("%d cores", spec.CPUCores)
	}

	memory := spec.Memory
	if strings.TrimSpace(memory) == "" {
		memory = "unknown"
	}

	gpus := formatGPUSummary(spec.GPUs)
	if strings.TrimSpace(gpus) == "" {
		gpus = "unknown"
	}

	return hostListRow{
		Type:   "host",
		Name:   spec.Name,
		OSArch: osArch,
		CPU:    cpu,
		Memory: memory,
		GPUs:   gpus,
	}
}

func hostListUnknownRow(name string) hostListRow {
	return hostListRow{
		Type:   "host",
		Name:   name,
		OSArch: "unknown/unknown",
		CPU:    "unknown",
		Memory: "unknown",
		GPUs:   "unknown",
	}
}

func runHostDiscover(cmd *cobra.Command, args []string) error {
	host := args[0]

	if isInstanceIDArg(host) {
		return fmt.Errorf("weft host discover writes inventory YAMLs for on-prem hosts; cloud instances are ephemeral and not persisted to the inventory")
	}

	fmt.Fprintf(os.Stderr, "Probing %s via SSH...\n", host)
	_, yamlPath, err := discoverHost(host)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Wrote %s\n", yamlPath)
	return nil
}

func formatGPUSummary(gpus []inventory.GPUSpec) string {
	if len(gpus) == 0 {
		return "none"
	}
	var parts []string
	for _, g := range gpus {
		parts = append(parts, fmt.Sprintf("%dx %s (%s)", len(g.Indices), g.Name, g.Memory))
	}
	return strings.Join(parts, ", ")
}
