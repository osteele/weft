package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
	"github.com/osteele/weft/internal/ssh"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
	"github.com/spf13/cobra"
)

var (
	dataJSON         bool
	dataFetchHost    string
	dataFetchRev     string
	dataRequestsHost string

	dataAddHost string
	dataAddName string

	dataPublishName       string
	dataPublishTargetPath string
	dataPublishHost       string

	dataEvictHost     string
	dataEvictLastUsed string
	dataEvictDryRun   bool
	dataEvictYes      bool
)

var dataCmd = &cobra.Command{
	Use:   "data",
	Short: "Query and manage shared data assets on hosts",
	Long: `Query which hosts have HuggingFace models and datasets cached, and
request a download onto a specific on-prem host.

Examples:
  weft data where hf:meta-llama/Llama-3-8B
  weft data fetch hf:meta-llama/Llama-3-8B --host cool100
  weft data fetch hf-dataset:HuggingFaceFW/fineweb --host cool30 --revision main
  weft data requests --host cool100`,
}

var dataWhereCmd = &cobra.Command{
	Use:   "where <asset-ref>",
	Short: "Show which hosts have a given asset",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runDataWhere,
}

var dataFetchCmd = &cobra.Command{
	Use:   "fetch <asset-ref>",
	Short: "Request and execute a download onto a host",
	Long: `Download a HuggingFace model or dataset onto a specific host, record the
request, and refresh the local asset inventory on success.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runDataFetch,
}

var dataRequestsCmd = &cobra.Command{
	Use:   "requests",
	Short: "List recorded data download requests",
	Args:  cobra.NoArgs,
	RunE:  runDataRequests,
}

var dataAddCmd = &cobra.Command{
	Use:   "add <path>",
	Short: "Register a checkpoint or dataset in the asset inventory",
	Long: `Register a local directory as a checkpoint asset so that it is visible to
placement scoring (--input checkpoint:<name>) and data locality queries
(weft data where checkpoint:<name>).

The asset name defaults to <repo-name>/<relative-path> when the path is inside
a repository, or the directory basename otherwise. Use --name to override.

The host defaults to the local machine's hostname. Use --host to register data
on a remote host.

Examples:
  weft data add ~/code/research/LM2/runs/gpt2-ft-v1
  weft data add ~/code/research/LM2/runs/gpt2-ft-v1 --host cool100
  weft data add ~/research/data/grads --host studio --name gpt2-grads-wikitext`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runDataAdd,
}

var dataPublishCmd = &cobra.Command{
	Use:   "publish [--host <host>] <path>",
	Short: "Publish a file as a named asset usable from any host",
	Long: `Upload a local or remote file to R2 under a stable name and register it in the
named_assets table. Jobs can then declare ` + "`--input asset:<name>`" + ` to consume
the file from any host (cloud rental or on-prem with R2 access), without
pinning placement to a particular host the way ` + "`checkpoint:`" + ` does.

The asset is content-addressed (assets/<sha256>) so republishing identical
bytes is a no-op upload. Republishing under the same name with different
bytes overwrites the name → hash mapping; the previous blob is left in R2.

At consumer-job launch time the file is staged into the working dir at the
relative path the local file lived at (or the path given by --target-path),
so the consumer script reads it the same way it would in the producer's
workspace.

Examples:
  weft data publish output/exp207_eval_texts.pkl --name exp207-eval-llama8b
  weft data publish ~/data/grads.bin --name lm2-grads-v3 --target-path data/grads.bin
  weft data publish --host cool30 /data/traces/toolagent.jsonl --name toolagent-trace --target-path data/toolagent.jsonl
  weft data publish cool30:/data/traces/toolagent.jsonl --name toolagent-trace --target-path data/toolagent.jsonl
`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runDataPublish,
}

var dataEvictCmd = &cobra.Command{
	Use:   "evict",
	Short: "Evict HuggingFace cache items not accessed recently",
	Long: `Evict HuggingFace cache directories from remote hosts based on last filesystem
access time, and remove them from the local asset inventory.

Respects HF_HUB_CACHE and HF_HOME on the remote host.

Examples:
  weft data evict --last-used 90d --dry-run
  weft data evict --host cool100 --last-used 30d
  weft data evict --last-used 60d --yes`,
	Args: cobra.NoArgs,
	RunE: runDataEvict,
}

var (
	dataPublishBuildR2Client = func(cfg *config.Config) (dataPublishR2Client, error) {
		return buildR2Client(cfg)
	}
	dataPublishCopyFrom = ssh.CopyFromWithRetry
)

func init() {
	rootCmd.AddCommand(dataCmd)
	dataCmd.PersistentFlags().BoolVar(&dataJSON, "json", false, "Print machine-readable JSON output")
	dataCmd.AddCommand(dataWhereCmd)
	dataCmd.AddCommand(dataFetchCmd)
	dataCmd.AddCommand(dataRequestsCmd)
	dataCmd.AddCommand(dataAddCmd)
	dataCmd.AddCommand(dataPublishCmd)
	dataCmd.AddCommand(dataEvictCmd)

	dataAddCmd.Flags().StringVar(&dataAddHost, "host", "", "Host where the data lives (default: local hostname)")
	dataAddCmd.Flags().StringVar(&dataAddName, "name", "", "Asset name (default: derived from repo-relative path)")

	dataPublishCmd.Flags().StringVar(&dataPublishName, "name", "", "Stable name to publish under (required)")
	dataPublishCmd.Flags().StringVar(&dataPublishTargetPath, "target-path", "", "Workspace-relative path to stage into at consumer launch time (default: derived from local path)")
	dataPublishCmd.Flags().StringVar(&dataPublishHost, "host", "", "Remote host where the source file lives")
	_ = dataPublishCmd.MarkFlagRequired("name")

	dataFetchCmd.Flags().StringVar(&dataFetchHost, "host", "", "On-prem host that should cache the asset")
	dataFetchCmd.Flags().StringVar(&dataFetchRev, "revision", "main", "HF revision to download")
	dataFetchCmd.MarkFlagRequired("host")

	dataRequestsCmd.Flags().StringVar(&dataRequestsHost, "host", "", "Filter recorded requests by host")

	dataEvictCmd.Flags().StringVar(&dataEvictHost, "host", "", "Restrict to a specific host (default: all inventory hosts)")
	dataEvictCmd.Flags().StringVar(&dataEvictLastUsed, "last-used", "", "Evict items not seen in the inventory for longer than this duration (e.g. 90d, 720h)")
	dataEvictCmd.Flags().BoolVar(&dataEvictDryRun, "dry-run", false, "Preview what would be evicted without making changes")
	dataEvictCmd.Flags().BoolVar(&dataEvictYes, "yes", false, "Skip confirmation prompt")
	_ = dataEvictCmd.MarkFlagRequired("last-used")
}

func runDataWhere(_ *cobra.Command, args []string) error {
	asset, err := parseDataAssetArg(args[0])
	if err != nil {
		return err
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	entries, err := dataloc.FindAssetHosts(database, asset)
	if err != nil {
		return fmt.Errorf("find asset hosts: %w", err)
	}

	if dataJSON {
		return json.NewEncoder(os.Stdout).Encode(entries)
	}

	if len(entries) == 0 {
		fmt.Printf("No hosts are known to have %s\n", asset)
		fmt.Println("Run `weft host data <host> --scan` or `weft data fetch` to populate the inventory.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "HOST\tSIZE\tLAST SEEN\tPATH\n")
	for _, entry := range entries {
		path := entry.Path
		if path == "" {
			path = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s ago\t%s\n",
			entry.Host,
			formatBytes(entry.SizeBytes),
			db.FormatDuration(int64(time.Since(entry.LastSeen).Seconds())),
			path,
		)
	}
	return w.Flush()
}

func runDataFetch(_ *cobra.Command, args []string) error {
	asset, err := parseDataAssetArg(args[0])
	if err != nil {
		return err
	}
	if !isValidDataFetchHost(dataFetchHost) {
		return fmt.Errorf("host %q not found in inventory", dataFetchHost)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	request, err := dataloc.CreateDataRequest(database, dataloc.CreateRequestParams{
		Host:     dataFetchHost,
		Asset:    asset,
		Revision: dataFetchRev,
	})
	if err != nil {
		return fmt.Errorf("create data request: %w", err)
	}

	if err := dataloc.MarkDataRequestRunning(database, request.ID); err != nil {
		return fmt.Errorf("mark request running: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	entry, err := dataloc.DownloadAssetToHost(ctx, dataFetchHost, asset, dataFetchRev)
	if err != nil {
		if ferr := dataloc.MarkDataRequestFailed(database, request.ID, err.Error()); ferr != nil {
			slog.Warn("failed to mark data request as failed", "request_id", request.ID, "error", ferr)
		}
		return err
	}

	entry.LastSeen = time.Now().UTC().Truncate(time.Second)
	if err := dataloc.RecordAsset(database, entry); err != nil {
		if ferr := dataloc.MarkDataRequestFailed(database, request.ID, err.Error()); ferr != nil {
			slog.Warn("failed to mark data request as failed", "request_id", request.ID, "error", ferr)
		}
		return fmt.Errorf("record downloaded asset: %w", err)
	}
	if err := dataloc.MarkDataRequestCompleted(database, request.ID, entry); err != nil {
		return fmt.Errorf("mark request completed: %w", err)
	}

	request, err = dataloc.GetDataRequest(database, request.ID)
	if err != nil {
		return fmt.Errorf("reload request: %w", err)
	}

	if dataJSON {
		return json.NewEncoder(os.Stdout).Encode(request)
	}

	fmt.Printf("Request %d completed\n", request.ID)
	fmt.Printf("Host: %s\n", request.Host)
	fmt.Printf("Asset: %s\n", request.Asset)
	fmt.Printf("Revision: %s\n", request.Revision)
	if request.RemotePath != "" {
		fmt.Printf("Path: %s\n", request.RemotePath)
	}
	fmt.Printf("Size: %s\n", formatBytes(request.SizeBytes))
	return nil
}

func runDataRequests(_ *cobra.Command, _ []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	requests, err := dataloc.ListDataRequests(database, dataRequestsHost)
	if err != nil {
		return fmt.Errorf("list data requests: %w", err)
	}

	if dataJSON {
		return json.NewEncoder(os.Stdout).Encode(requests)
	}

	if len(requests) == 0 {
		if dataRequestsHost == "" {
			fmt.Println("No recorded data requests")
		} else {
			fmt.Printf("No recorded data requests for %s\n", dataRequestsHost)
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tHOST\tASSET\tREVISION\tSTATUS\tREQUESTED\tSIZE\n")
	for _, request := range requests {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s ago\t%s\n",
			request.ID,
			request.Host,
			request.Asset,
			request.Revision,
			request.Status,
			db.FormatDuration(int64(time.Since(request.RequestedAt).Seconds())),
			formatBytes(request.SizeBytes),
		)
	}
	return w.Flush()
}

func runDataAdd(_ *cobra.Command, args []string) error {
	path := filepath.Clean(runner.ExpandTilde(args[0]))

	host := dataAddHost
	if host == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("detect hostname: %w", err)
		}
		host = h
	}

	name := dataAddName
	if name == "" {
		name = deriveCheckpointName(path)
	}

	asset := dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: name}
	entry := dataloc.HostDataEntry{
		Host:     host,
		Asset:    asset,
		Path:     workdir.ToTildeRelative(path),
		LastSeen: time.Now().UTC().Truncate(time.Second),
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if err := dataloc.RecordAsset(database, entry); err != nil {
		return fmt.Errorf("record asset: %w", err)
	}

	fmt.Printf("Registered checkpoint:%s on %s (%s)\n", name, host, entry.Path)
	return nil
}

// deriveCheckpointName derives an asset ID from a filesystem path.
// If the path is inside a repository, returns "<repo-basename>/<relative-path>".
// Otherwise returns the path's basename.
func deriveCheckpointName(path string) string {
	// DetectRepoRoot needs a directory; try path first, then its parent
	// in case path is a regular file.
	root := workdir.DetectRepoRoot(path)
	if root == "" {
		root = workdir.DetectRepoRoot(filepath.Dir(path))
	}
	if root != "" {
		rel, err := filepath.Rel(root, path)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Base(root) + "/" + rel
		}
	}
	return filepath.Base(path)
}

func parseDataAssetArg(arg string) (dataloc.DataAsset, error) {
	asset, ok := dataloc.ParseAssetRef(arg)
	if !ok {
		return dataloc.DataAsset{}, usageErrorf("invalid asset ref %q (expected hf:<repo>, hf-dataset:<repo>, checkpoint:<name>, or job-output:<name>)", arg)
	}
	return asset, nil
}

func isValidDataFetchHost(host string) bool {
	return inventory.FindHost(host) != nil || srcsync.IsLocalHost(host)
}

func runDataEvict(_ *cobra.Command, _ []string) error {
	duration, err := parseDuration(dataEvictLastUsed)
	if err != nil {
		return usageErrorf("invalid --last-used value %q: %v", dataEvictLastUsed, err)
	}
	cutoff := time.Now().Add(-duration)

	if dataEvictHost != "" && !isValidDataFetchHost(dataEvictHost) {
		return fmt.Errorf("host %q not found in inventory", dataEvictHost)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	candidates, err := dataloc.FindAssetsNotUsedSince(database, dataEvictHost, cutoff)
	if err != nil {
		return fmt.Errorf("query unused assets: %w", err)
	}

	// Fill in missing sizes with a live scan, grouped by host.
	needScan := map[string]bool{}
	for _, e := range candidates {
		if e.SizeBytes == 0 && e.Path != "" {
			needScan[e.Host] = true
		}
	}
	if len(needScan) > 0 {
		liveSizes := map[string]int64{} // path -> bytes
		for host := range needScan {
			entries, err := dataloc.ScanHFCacheDetailed(host)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: size scan %s: %v\n", host, err)
				continue
			}
			for _, e := range entries {
				liveSizes[e.Path] = e.SizeBytes
			}
		}
		for i := range candidates {
			if candidates[i].SizeBytes == 0 && candidates[i].Path != "" {
				candidates[i].SizeBytes = liveSizes[candidates[i].Path]
			}
		}
	}

	if len(candidates) == 0 {
		fmt.Println("Nothing to evict.")
		return nil
	}

	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "warning: load config: %v\n", cfgErr)
	}
	policy := config.CacheEvictionPolicyLRU
	reuseWindow := 30 * 24 * time.Hour
	if cfg != nil {
		policy = cfg.CacheEvictionPolicy()
		reuseWindow = cfg.CacheReuseWindow()
	}
	recentCounts := map[string]int{}
	if policy == config.CacheEvictionPolicyReusePerGB {
		counts, err := dataloc.RecentAssetUseCounts(database, candidates, time.Now().Add(-reuseWindow))
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: reuse-per-GB counts unavailable; using zero counts: %v\n", err)
		} else {
			recentCounts = counts
		}
	}
	dataloc.SortEvictionCandidates(candidates, policy, recentCounts)

	// Build map of asset -> other on-prem hosts that have it.
	inventoryHosts, err := inventory.LoadHosts()
	if err != nil {
		return fmt.Errorf("load inventory: %w", err)
	}
	inventoryHostSet := map[string]bool{}
	for _, h := range inventoryHosts {
		inventoryHostSet[h.Name] = true
	}
	allEntries, err := dataloc.ListAllAssets(database)
	if err != nil {
		return fmt.Errorf("list assets: %w", err)
	}
	type assetKey struct{ kind, id string }
	assetToHosts := map[assetKey][]string{} // asset -> all on-prem hosts with it
	for _, e := range allEntries {
		if !inventoryHostSet[e.Host] {
			continue
		}
		k := assetKey{string(e.Asset.Kind), e.Asset.ID}
		assetToHosts[k] = append(assetToHosts[k], e.Host)
	}
	// For each candidate, find other on-prem hosts (excluding the candidate's own host).
	elsewhere := make([][]string, len(candidates))
	for i, c := range candidates {
		k := assetKey{string(c.Asset.Kind), c.Asset.ID}
		for _, h := range assetToHosts[k] {
			if h != c.Host {
				elsewhere[i] = append(elsewhere[i], h)
			}
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "HOST\tASSET\tSIZE\tLAST USED\tELSEWHERE\n")
	var totalBytes, safeBytes int64
	var safeCount int
	for i, e := range candidates {
		lastUsed := "never"
		if !e.LastUsedAt.IsZero() {
			lastUsed = db.FormatDuration(int64(time.Since(e.LastUsedAt).Seconds())) + " ago"
		}
		elsewhereCol := "—"
		if len(elsewhere[i]) > 0 {
			elsewhereCol = strings.Join(elsewhere[i], ", ")
			safeBytes += e.SizeBytes
			safeCount++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			e.Host, e.Asset,
			formatBytes(e.SizeBytes),
			lastUsed,
			elsewhereCol,
		)
		totalBytes += e.SizeBytes
	}
	w.Flush()
	fmt.Printf("\nTotal: %d item(s), %s", len(candidates), formatBytes(totalBytes))
	if safeCount > 0 && safeCount < len(candidates) {
		fmt.Printf(" (%d available elsewhere: %s)", safeCount, formatBytes(safeBytes))
	}
	fmt.Println()

	if dataEvictDryRun {
		fmt.Println("(dry run — no changes made)")
		return nil
	}

	// Determine which candidates to evict based on user choice.
	var toEvict []dataloc.HostDataEntryWithUsage
	if dataEvictYes {
		toEvict = candidates
	} else {
		choice := evictPrompt(len(candidates), safeCount)
		switch choice {
		case "a":
			toEvict = candidates
		case "s":
			for i, e := range candidates {
				if len(elsewhere[i]) > 0 {
					toEvict = append(toEvict, e)
				}
			}
		default:
			fmt.Println("Aborted.")
			return nil
		}
	}

	var evicted, failed int
	var freedBytes int64
	for _, e := range toEvict {
		if e.Path == "" {
			fmt.Fprintf(os.Stderr, "warning: no path for %s on %s; skipping remote deletion\n", e.Asset, e.Host)
		} else if err := dataloc.EvictAsset(e.Host, e.Path); err != nil {
			fmt.Fprintf(os.Stderr, "error: evict %s on %s: %v\n", e.Asset, e.Host, err)
			failed++
			continue
		}
		if err := dataloc.DeleteHostDataEntry(database, e.Host, e.Asset); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove DB entry for %s on %s: %v\n", e.Asset, e.Host, err)
		}
		evicted++
		freedBytes += e.SizeBytes
	}
	freed := formatBytes(freedBytes)
	if failed > 0 {
		freed = "~" + freed
	}
	fmt.Printf("Evicted %d item(s), freed %s\n", evicted, freed)
	if failed > 0 {
		return fmt.Errorf("%d eviction(s) failed", failed)
	}
	return nil
}

// evictPrompt asks the user what to evict and returns "a" (all), "s" (safe/elsewhere only), or "".
func evictPrompt(total, safeCount int) string {
	var r string
	if safeCount < total && safeCount > 0 {
		// Mixed: some available elsewhere, some not — offer all, safe-only, or cancel.
		fmt.Printf("Evict: [a]ll %d item(s), [s]afe %d (available elsewhere), [N]o cancel? ",
			total, safeCount)
		fmt.Scanln(&r)
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "a":
			return "a"
		case "s":
			return "s"
		default:
			return ""
		}
	}
	// All items are in the same category (none or all available elsewhere).
	if safeCount == total {
		fmt.Printf("Evict all %d item(s) (all available elsewhere)? [y/N] ", total)
	} else {
		fmt.Printf("Evict all %d item(s)? [y/N] ", total)
	}
	fmt.Scanln(&r)
	if strings.ToLower(strings.TrimSpace(r)) == "y" {
		return "a"
	}
	return ""
}

func formatBytes(size int64) string {
	if size <= 0 {
		return "-"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(size)
	unit := units[0]
	for i := 1; i < len(units) && value >= 1024; i++ {
		value /= 1024
		unit = units[i]
	}
	if unit == "B" {
		return fmt.Sprintf("%d%s", size, unit)
	}
	formatted := fmt.Sprintf("%.1f", value)
	formatted = strings.TrimSuffix(formatted, ".0")
	return formatted + unit
}

func runDataPublish(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(dataPublishName)
	if name == "" {
		return fmt.Errorf("--name is required")
	}
	if strings.ContainsAny(name, " \t\n/") {
		return fmt.Errorf("--name %q: must not contain whitespace or slashes", name)
	}

	source, err := resolvePublishSource(args[0])
	if err != nil {
		return err
	}
	srcPath := source.LocalPath
	if source.Cleanup != nil {
		defer source.Cleanup()
	}

	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", srcPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("publish %s: directories are not supported in v1 (publish a single file)", srcPath)
	}

	targetPath := strings.TrimSpace(dataPublishTargetPath)
	if targetPath == "" {
		if source.Remote {
			return fmt.Errorf("remote publish requires --target-path")
		}
		targetPath = derivePublishTargetPath(srcPath)
	}
	targetPath = strings.TrimPrefix(targetPath, "/")
	if targetPath == "" {
		return fmt.Errorf("could not derive a workspace-relative target path; pass --target-path")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	r2Client, err := dataPublishBuildR2Client(cfg)
	if err != nil {
		return fmt.Errorf("build R2 client: %w", err)
	}
	if r2Client == nil {
		return fmt.Errorf("R2 is not configured; named assets require R2 to stage onto consumer hosts")
	}

	sumHex, size, err := sha256File(srcPath)
	if err != nil {
		return fmt.Errorf("hash %s: %w", srcPath, err)
	}
	key := r2keys.NamedAsset(sumHex)

	ctx := context.Background()
	if cmd != nil && cmd.Context() != nil {
		ctx = cmd.Context()
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Skip the upload if the content is already in R2 (content-addressed).
	exists, err := r2Client.ObjectExists(ctx, key)
	if err != nil {
		return fmt.Errorf("probe R2 for existing %s: %w", key, err)
	}
	if !exists {
		if err := uploadFileToR2(ctx, r2Client, srcPath, key); err != nil {
			return fmt.Errorf("upload %s to R2: %w", srcPath, err)
		}
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	if err := db.UpsertNamedAsset(database, db.NamedAsset{
		Name:        name,
		ContentHash: sumHex,
		SizeBytes:   size,
		TargetPath:  targetPath,
	}); err != nil {
		return err
	}

	action := "uploaded"
	if exists {
		action = "registered (bytes already in R2)"
	}
	out := io.Writer(os.Stdout)
	if cmd != nil {
		out = cmd.OutOrStdout()
	}
	fmt.Fprintf(out, "Published asset:%s — %s, %s (target path %s)\n",
		name, action, formatBytes(size), targetPath)
	fmt.Fprintf(out, "Consumers: weft run --input asset:%s ...\n", name)
	return nil
}

type publishSource struct {
	LocalPath string
	Remote    bool
	Cleanup   func()
}

func resolvePublishSource(raw string) (publishSource, error) {
	flagHost := strings.TrimSpace(dataPublishHost)
	if host, path, ok := parseRemotePublishArg(raw); ok {
		if flagHost != "" {
			return publishSource{}, fmt.Errorf("cannot combine --host with host:path source")
		}
		return copyRemotePublishSource(host, path)
	}
	if flagHost != "" {
		return copyRemotePublishSource(flagHost, raw)
	}
	return publishSource{LocalPath: filepath.Clean(runner.ExpandTilde(raw))}, nil
}

func parseRemotePublishArg(raw string) (host, path string, ok bool) {
	before, after, found := strings.Cut(raw, ":")
	if !found || before == "" || after == "" {
		return "", "", false
	}
	if strings.Contains(before, "/") || strings.HasPrefix(before, ".") {
		return "", "", false
	}
	return before, after, true
}

func copyRemotePublishSource(host, remotePath string) (publishSource, error) {
	host = strings.TrimSpace(host)
	remotePath = strings.TrimSpace(remotePath)
	if host == "" {
		return publishSource{}, fmt.Errorf("--host is required for remote publish")
	}
	if remotePath == "" {
		return publishSource{}, fmt.Errorf("remote path is required")
	}
	tmp, err := os.CreateTemp("", "weft-data-publish-*")
	if err != nil {
		return publishSource{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return publishSource{}, fmt.Errorf("close temp file: %w", err)
	}
	if err := dataPublishCopyFrom(remotePath, host, tmpPath); err != nil {
		os.Remove(tmpPath)
		return publishSource{}, fmt.Errorf("copy %s:%s: %w", host, remotePath, err)
	}
	return publishSource{
		LocalPath: tmpPath,
		Remote:    true,
		Cleanup: func() {
			if err := os.Remove(tmpPath); err != nil {
				slog.Warn("remove temp publish file", "path", tmpPath, "error", err)
			}
		},
	}, nil
}

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), size, nil
}

func uploadFileToR2(ctx context.Context, client dataPublishR2Client, path, key string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return client.PutObject(ctx, key, f, "application/octet-stream")
}

// dataPublishR2Client is the minimal R2 surface data publishing needs (allows
// tests to substitute without dragging in the real S3 client).
type dataPublishR2Client interface {
	ObjectExists(ctx context.Context, key string) (bool, error)
	PutObject(ctx context.Context, key string, body io.Reader, contentType string) error
}

// derivePublishTargetPath chooses where the asset stages into a consumer's
// workdir when --target-path is not given. Inside a repo: the path relative
// to the repo root, so a file at <repo>/output/X.pkl stages back to
// output/X.pkl on the consumer. Outside a repo: just the basename.
func derivePublishTargetPath(srcPath string) string {
	root := workdir.DetectRepoRoot(srcPath)
	if root == "" {
		root = workdir.DetectRepoRoot(filepath.Dir(srcPath))
	}
	if root != "" {
		rel, err := filepath.Rel(root, srcPath)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(srcPath)
}
