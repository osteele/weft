package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/spf13/cobra"
)

var (
	dataJSON         bool
	dataFetchHost    string
	dataFetchRev     string
	dataRequestsHost string
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

func init() {
	rootCmd.AddCommand(dataCmd)
	dataCmd.PersistentFlags().BoolVar(&dataJSON, "json", false, "Print machine-readable JSON output")
	dataCmd.AddCommand(dataWhereCmd)
	dataCmd.AddCommand(dataFetchCmd)
	dataCmd.AddCommand(dataRequestsCmd)

	dataFetchCmd.Flags().StringVar(&dataFetchHost, "host", "", "On-prem host that should cache the asset")
	dataFetchCmd.Flags().StringVar(&dataFetchRev, "revision", "main", "HF revision to download")
	dataFetchCmd.MarkFlagRequired("host")

	dataRequestsCmd.Flags().StringVar(&dataRequestsHost, "host", "", "Filter recorded requests by host")
}

func runDataWhere(_ *cobra.Command, args []string) error {
	asset, err := parseDataAssetArg(args[0])
	if err != nil {
		return err
	}

	database, err := db.Open()
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
			log.Printf("warning: mark data request %d failed: %v", request.ID, ferr)
		}
		return err
	}

	entry.LastSeen = time.Now().UTC().Truncate(time.Second)
	if err := dataloc.RecordAsset(database, entry); err != nil {
		if ferr := dataloc.MarkDataRequestFailed(database, request.ID, err.Error()); ferr != nil {
			log.Printf("warning: mark data request %d failed: %v", request.ID, ferr)
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
	database, err := db.Open()
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

func parseDataAssetArg(arg string) (dataloc.DataAsset, error) {
	asset, ok := dataloc.ParseAssetRef(arg)
	if !ok {
		return dataloc.DataAsset{}, usageErrorf("invalid asset ref %q (expected hf:<repo> or hf-dataset:<repo>)", arg)
	}
	switch asset.Kind {
	case dataloc.AssetHFModel, dataloc.AssetHFDataset:
		return asset, nil
	default:
		return dataloc.DataAsset{}, usageErrorf("asset ref %q is not a supported downloadable HF asset", arg)
	}
}

func isValidDataFetchHost(host string) bool {
	return inventory.FindHost(host) != nil || srcsync.IsLocalHost(host)
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
