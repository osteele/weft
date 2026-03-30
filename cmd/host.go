package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/util"
	"github.com/spf13/cobra"
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
	Long: `List all known on-prem hosts from inventory and recent local state, with OS, architecture, and GPU specs.

Example:
  weft host list`,
	Args: cobra.NoArgs,
	RunE: runHostList,
}

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
	hostCmd.AddCommand(hostSetupCmd)

	hostDataCmd.Flags().BoolVar(&hostDataScan, "scan", false, "Scan remote HF cache and update local database")
}

func runHostInfo(cmd *cobra.Command, args []string) error {
	host := args[0]

	// Handle rental:NN format — delegate to instance status
	if instanceID, ok := strings.CutPrefix(host, "rental:"); ok {
		return runInstanceStatus(cmd, []string{instanceID})
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Try to get cached host info first
	cachedInfo, err := db.LoadCachedHostInfo(database, host)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("load cached info: %w", err)
	}

	// Display cached info if available
	if cachedInfo != nil {
		displayHostInfo(host, cachedInfo)
		cacheAge := time.Now().Unix() - cachedInfo.LastUpdated
		fmt.Printf("\n(cached %s ago)\n", db.FormatDuration(cacheAge))
	} else {
		fmt.Printf("No cached information for %s\n", host)
		fmt.Printf("Run 'weft host discover %s' to probe and cache host information\n", host)
	}

	return nil
}

func displayHostInfo(host string, info *db.CachedHostInfo) {
	fmt.Printf("Host: %s\n", host)
	if info.Arch != "" {
		fmt.Printf("Architecture: %s\n", info.Arch)
	}
	if info.Model != "" {
		fmt.Printf("Model: %s\n", info.Model)
	}
	if info.OSVersion != "" {
		fmt.Printf("OS: %s\n", info.OSVersion)
	}
	if info.CPUCount > 0 {
		fmt.Printf("CPUs: %d", info.CPUCount)
		if info.CPUModel != "" {
			fmt.Printf(" (%s", info.CPUModel)
			if info.CPUFreq != "" {
				fmt.Printf(" @ %s", info.CPUFreq)
			}
			fmt.Printf(")")
		}
		fmt.Println()
	}
	if info.MemTotal != "" {
		fmt.Printf("Memory: %s\n", info.MemTotal)
	}

	// Parse and display GPUs from JSON
	if info.GPUsJSON != "" {
		fmt.Printf("\nGPUs: %s\n", info.GPUsJSON)
	}
}

func runHostJobs(cmd *cobra.Command, args []string) error {
	host := args[0]

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get all active jobs for this host
	jobs, err := db.ListActiveJobs(database, host)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	if len(jobs) == 0 {
		fmt.Printf("No active jobs on %s\n", host)
		return nil
	}

	// Display jobs in a table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tSTATUS\tSTARTED\tCOMMAND / DESCRIPTION\n")

	for _, job := range jobs {
		started := time.Unix(job.StartTime, 0).Format("01/02 15:04")

		display := job.Description
		if display == "" {
			display = job.EffectiveCommand()
		}
		if len(display) > 50 {
			display = display[:47] + "..."
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n",
			job.ID, job.Status, started, display)
	}

	w.Flush()
	fmt.Printf("\nTotal: %d active job(s) on %s\n", len(jobs), host)

	return nil
}

func runHostLoad(cmd *cobra.Command, args []string) error {
	host := args[0]

	fmt.Printf("Fetching current load for %s...\n", host)

	// Get uptime and load average
	uptimeCmd := "uptime"
	stdout, _, err := ssh.Run(host, uptimeCmd)
	if err != nil {
		return fmt.Errorf("get uptime: %w", err)
	}

	fmt.Printf("\nHost: %s\n", host)
	fmt.Printf("Uptime: %s\n", strings.TrimSpace(stdout))

	// Get memory info
	memCmd := "free -h | grep Mem"
	stdout, _, err = ssh.Run(host, memCmd)
	if err == nil {
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
	stdout, _, err = ssh.Run(host, gpuCmd)
	if err == nil && strings.TrimSpace(stdout) != "" {
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

	if hostDataScan {
		fmt.Printf("Scanning HuggingFace cache on %s...\n", host)
		entries, err := dataloc.ScanHFCacheDetailed(host)
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
	rows, err := loadHostListRows(time.Now())
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "NAME\tOS/ARCH\tCPU\tMEMORY\tGPUs\n")
	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			row.Name, row.OSArch, row.CPU, row.Memory, row.GPUs)
	}
	return w.Flush()
}

type hostListRow struct {
	Name   string
	OSArch string
	CPU    string
	Memory string
	GPUs   string
}

func loadHostListRows(now time.Time) ([]hostListRow, error) {
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

	return rows, nil
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
		Name:   spec.Name,
		OSArch: osArch,
		CPU:    cpu,
		Memory: memory,
		GPUs:   gpus,
	}
}

func hostListUnknownRow(name string) hostListRow {
	return hostListRow{
		Name:   name,
		OSArch: "unknown/unknown",
		CPU:    "unknown",
		Memory: "unknown",
		GPUs:   "unknown",
	}
}

func runHostDiscover(cmd *cobra.Command, args []string) error {
	host := args[0]

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
