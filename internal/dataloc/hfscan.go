package dataloc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// scanBaseDirSentinel is printed to stderr by the HF and corpus scan scripts
// when their base directory is missing or unreadable. It lets the Go caller
// distinguish "looked and the directory is genuinely empty" (exit 0, no output,
// pruning stale rows is safe) from "could not look — the base directory is
// absent or unmounted" (exit 3, this sentinel, pruning must be skipped). See
// the "Absence of Evidence Is Not Evidence of Absence" rules in CLAUDE.md.
const scanBaseDirSentinel = "WEFT_SCAN_BASE_UNAVAILABLE"

// ErrScanBaseDirUnavailable indicates a host scan could not enumerate its base
// directory because that directory was missing or unreadable. An empty result
// paired with this error is "unknown", not "confirmed absent": callers MUST NOT
// prune data-locality rows on this signal, since the assets may live on a
// transiently unmounted or unreachable volume.
var ErrScanBaseDirUnavailable = errors.New("scan base directory unavailable")

// classifyScanError maps a scan command's (stderr, err) into either the
// base-dir-unavailable sentinel error or the original error wrapped with
// context. A nil err is returned unchanged.
func classifyScanError(what, host, stderr string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(stderr, scanBaseDirSentinel) {
		return fmt.Errorf("%s on %s: %w", what, host, ErrScanBaseDirUnavailable)
	}
	return fmt.Errorf("%s on %s: %w", what, host, err)
}

// ScanHFCache scans the HuggingFace cache on a remote host and returns
// discovered models and datasets. It looks for directories matching the
// HF cache layout: ~/.cache/huggingface/hub/models--<org>--<name>
// and ~/.cache/huggingface/hub/datasets--<org>--<name>.
func ScanHFCache(host string) ([]DataAsset, error) {
	entries, err := ScanHFCacheDetailed(host)
	if err != nil {
		return nil, err
	}
	assets := make([]DataAsset, len(entries))
	for i, e := range entries {
		assets[i] = e.Asset
	}
	return assets, nil
}

// ScanHFCacheDetailed scans the HuggingFace cache on a remote host and returns
// entries with full path and size information for each well-formed asset.
// Entries that fail structural validation (doubly-nested cache, missing
// snapshots/) or that the weft user cannot read (owned by another user, e.g.
// a root-context container download into a shared cache) are logged and
// dropped — they are not loadable via the normal HF resolver. Sizes exclude
// *.incomplete blobs (partial downloads).
func ScanHFCacheDetailed(host string) ([]HostDataEntry, error) {
	return ScanHFCacheDetailedContext(context.Background(), host)
}

// ScanHFCacheDetailedContext is ScanHFCacheDetailed with caller-provided
// cancellation and timeout control for the remote scan command.
func ScanHFCacheDetailedContext(ctx context.Context, host string) ([]HostDataEntry, error) {
	results, err := scanHFCacheResultsContext(ctx, host)
	if err != nil {
		return nil, err
	}
	entries := make([]HostDataEntry, 0, len(results))
	for _, r := range results {
		if hfScanPresenceOf(r.Status) == hfScanAbsent {
			// Structurally invalid (doubly-nested cache or missing snapshots/):
			// the scan looked inside and positively determined this is not a
			// loadable HF asset. Drop it and let the pruner remove any stale row.
			slog.Debug("skipping malformed HF cache entry",
				"host", host, "asset", r.Asset.Ref(), "path", r.Path, "status", r.Status)
			continue
		}
		// Present (ok/incomplete) or unreadable. "unreadable" means the entry
		// directory exists but the weft user could not read it (ownership or
		// permission) — the bytes are on disk, so this is "unknown", not
		// "absent". Emit the entry so its last_seen is refreshed and the pruner
		// spares the row; never delete a data-locality row (and force a multi-GB
		// re-download) because a permission bit blocked the scan.
		entries = append(entries, hostDataEntryFromScanResult(host, r))
	}
	return entries, nil
}

// hfScanPresence classifies an HF scan status into whether the enumerated cache
// directory should be treated as present on the host.
type hfScanPresence int

const (
	hfScanPresent    hfScanPresence = iota // valid, readable cache entry (ok / incomplete)
	hfScanUnreadable                       // directory present but unreadable — unknown, not absent
	hfScanAbsent                           // present but malformed (nested / no-snapshots) → not loadable
)

// hfScanPresence classifies a (possibly comma-joined) status string. "unreadable"
// dominates: if the scan could not read the directory we never treat it as a
// confirmed-malformed asset. Only "nested"/"no-snapshots" — cases where the scan
// DID look inside and found the entry is not a usable HF asset — are absent.
func hfScanPresenceOf(status string) hfScanPresence {
	parts := strings.Split(status, ",")
	for _, p := range parts {
		if strings.TrimSpace(p) == "unreadable" {
			return hfScanUnreadable
		}
	}
	for _, p := range parts {
		switch strings.TrimSpace(p) {
		case "nested", "no-snapshots":
			return hfScanAbsent
		}
	}
	return hfScanPresent
}

func scanHFCacheResultsContext(ctx context.Context, host string) ([]hfScanResult, error) {
	cmd := hfCacheScanCommand()
	stdout, stderr, err := hostCommandRunner(ctx, host, cmd)
	if err != nil {
		return nil, classifyScanError("scan HF cache", host, stderr, err)
	}
	return parseHFCacheDetailedOutput(stdout), nil
}

func hfCacheScanCommand() string {
	return ResolveHFCacheDirShellVar() + `
if [ ! -d "$_hf_cache" ] || [ ! -r "$_hf_cache" ] || [ ! -x "$_hf_cache" ]; then echo "` + scanBaseDirSentinel + ` $_hf_cache" >&2; exit 3; fi
# POSIX: accumulate in the positional parameters. Arrays are a bash
# extension and this runs under /bin/sh, which is dash on Debian family.
set --
for _p in "$_hf_cache"/models--* "$_hf_cache"/datasets--*; do [ -d "$_p" ] && set -- "$@" "$_p"; done
[ "$#" -eq 0 ] && exit 0
for _d in "$@"; do
  _name=$(basename "$_d")
  _status="ok"
  if [ ! -r "$_d" ] || [ ! -x "$_d" ]; then
    _status="unreadable"
  elif [ ! -d "$_d/snapshots" ]; then
    if [ -d "$_d/$_name" ]; then
      _status="nested"
    else
      _status="no-snapshots"
    fi
  fi
  _bytes=""
  if _g=$(du -sb "$_d" 2>/dev/null) && [ -n "$_g" ]; then
    _bytes=$(printf '%s' "$_g" | awk 'NR==1{print $1}')
    if [ "$_status" = "ok" ] && [ -d "$_d/$_name" ]; then
      _nested=$(du -sb "$_d/$_name" 2>/dev/null | awk 'NR==1{print $1}')
      if [ -n "$_nested" ] && [ "$_nested" != "0" ]; then _bytes=$((_bytes - _nested)); fi
    fi
    if [ "$_status" = "ok" ] && [ -d "$_d/$_name" ]; then
      _inc=$(find "$_d" -path "$_d/$_name" -prune -o -name '*.incomplete' -type f -exec du -sb {} + 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')
    else
      _inc=$(find "$_d" -name '*.incomplete' -type f -exec du -sb {} + 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')
    fi
    if [ -n "$_inc" ] && [ "$_inc" != "0" ]; then
      _bytes=$((_bytes - _inc))
      if [ "$_status" = "ok" ]; then _status="incomplete"; else _status="$_status,incomplete"; fi
    fi
  fi
  if [ -z "$_bytes" ]; then
    if _g=$(du -sk "$_d" 2>/dev/null) && [ -n "$_g" ]; then
      _bytes=$(printf '%s' "$_g" | awk 'NR==1{printf "%d", $1*1024}')
      if [ "$_status" = "ok" ] && [ -d "$_d/$_name" ]; then
        _nested_kb=$(du -sk "$_d/$_name" 2>/dev/null | awk 'NR==1{print $1}')
        if [ -n "$_nested_kb" ] && [ "$_nested_kb" != "0" ]; then _bytes=$((_bytes - _nested_kb*1024)); fi
      fi
      if [ "$_status" = "ok" ] && [ -d "$_d/$_name" ]; then
        _inc_kb=$(find "$_d" -path "$_d/$_name" -prune -o -name '*.incomplete' -type f -exec du -sk {} + 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')
      else
        _inc_kb=$(find "$_d" -name '*.incomplete' -type f -exec du -sk {} + 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')
      fi
      if [ -n "$_inc_kb" ] && [ "$_inc_kb" != "0" ]; then
        _bytes=$((_bytes - _inc_kb*1024))
        if [ "$_status" = "ok" ]; then _status="incomplete"; else _status="$_status,incomplete"; fi
      fi
    fi
  fi
  printf '%s\t%s\t%s\n' "${_bytes:-0}" "$_status" "$_d"
done`
}

// hfScanResult is the parsed form of a single line emitted by the
// ScanHFCacheDetailed shell script. Status is one of "ok", "nested",
// "no-snapshots", "unreadable", "incomplete", or a comma-joined combination.
// "unreadable" means the entry directory exists but is not readable/searchable
// by the weft user (a cache ownership/permission issue, not a download fault).
type hfScanResult struct {
	Asset     DataAsset
	Path      string
	SizeBytes int64
	Status    string
}

func hostDataEntryFromScanResult(host string, r hfScanResult) HostDataEntry {
	return HostDataEntry{
		Host:      host,
		Asset:     r.Asset,
		Path:      r.Path,
		SizeBytes: r.SizeBytes,
	}
}

// parseHFCacheDetailedOutput parses lines of the form
// "<bytes>\t<status>\t<path>" emitted by the ScanHFCacheDetailed shell
// script. Lines with unrecognized HF directory names are skipped.
func parseHFCacheDetailedOutput(output string) []hfScanResult {
	var results []hfScanResult
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		sizeBytes, _ := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64)
		status := strings.TrimSpace(fields[1])
		path := strings.TrimSpace(fields[2])

		dirName := path
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			dirName = path[i+1:]
		}
		asset, ok := ParseHFDirName(dirName)
		if !ok {
			continue
		}
		results = append(results, hfScanResult{
			Asset:     asset,
			Path:      path,
			SizeBytes: sizeBytes,
			Status:    status,
		})
	}
	return results
}

// parseDuLine parses "<bytes>\t<path>" output from `du`, used by scanners
// other than HF (see corpusscan.go). Lines without a tab are treated as
// plain paths with size 0.
func parseDuLine(line string) (path string, sizeBytes int64) {
	if idx := strings.IndexByte(line, '\t'); idx > 0 {
		sizeBytes, _ = strconv.ParseInt(line[:idx], 10, 64)
		path = strings.TrimSpace(line[idx+1:])
	} else {
		path = line
	}
	return
}

// ParseHFDirName parses a HuggingFace cache directory name like
// "models--meta-llama--Llama-3-8B" into a DataAsset.
func ParseHFDirName(name string) (DataAsset, bool) {
	if strings.HasPrefix(name, "models--") {
		id := strings.TrimPrefix(name, "models--")
		return DataAsset{Kind: AssetHFModel, ID: hfDirToID(id)}, true
	}
	if strings.HasPrefix(name, "datasets--") {
		id := strings.TrimPrefix(name, "datasets--")
		return DataAsset{Kind: AssetHFDataset, ID: hfDirToID(id)}, true
	}
	return DataAsset{}, false
}

// hfDirToID converts a HF cache directory name component to a HF ID:
// HF uses "--" as the org/name separator in cache dir names.
func hfDirToID(dirPart string) string {
	return strings.Replace(dirPart, "--", "/", 1)
}
