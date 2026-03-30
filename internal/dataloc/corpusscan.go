package dataloc

import (
	"context"
	"fmt"
	"strings"
)

// CorpusBaseDir is the conventional base directory for shared corpora on hosts.
const CorpusBaseDir = ".local/share/corpora"

// ScanCorpusDir scans the corpus directory on a remote host and returns
// discovered corpus assets. It looks for directories under
// ~/.local/share/corpora/<collection>/<subset>/.
func ScanCorpusDir(host string) ([]HostDataEntry, error) {
	// List two-level subdirectories under the corpus base dir with sizes.
	// GNU du -sb first, then BSD du -sk fallback, then plain ls.
	cmd := fmt.Sprintf(`_corpus_dir="$HOME/%s"
[ -d "$_corpus_dir" ] || exit 0
_dirs=()
for _c in "$_corpus_dir"/*/; do
  [ -d "$_c" ] || continue
  for _s in "$_c"*/; do
    [ -d "$_s" ] && _dirs+=("$_s")
  done
done
[ ${#_dirs[@]} -eq 0 ] && exit 0
_out=$(du -sb "${_dirs[@]}" 2>/dev/null)
if [ -n "$_out" ]; then
  printf '%%s\n' "$_out"
else
  _out=$(du -sk "${_dirs[@]}" 2>/dev/null)
  if [ -n "$_out" ]; then
    printf '%%s\n' "$_out" | awk '{printf "%%d\t%%s\n", $1*1024, $2}'
  else
    printf '%%s\n' "${_dirs[@]}"
  fi
fi`, CorpusBaseDir)

	stdout, _, err := hostCommandRunner(context.Background(), host, cmd)
	if err != nil {
		return nil, fmt.Errorf("scan corpus dir on %s: %w", host, err)
	}
	return parseCorpusScanOutput(stdout, host), nil
}

// parseCorpusScanOutput parses du or ls output into HostDataEntries for corpus assets.
// Expects paths like ~/.local/share/corpora/<collection>/<subset>.
func parseCorpusScanOutput(output string, host string) []HostDataEntry {
	var entries []HostDataEntry
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		path, sizeBytes := parseDuLine(line)
		path = strings.TrimRight(path, "/")

		id := extractCorpusID(path)
		if id == "" {
			continue
		}

		entries = append(entries, HostDataEntry{
			Host:      host,
			Asset:     DataAsset{Kind: AssetCorpus, ID: id},
			Path:      path,
			SizeBytes: sizeBytes,
		})
	}
	return entries
}

// extractCorpusID extracts the "<collection>/<subset>" portion from a path
// ending in ...corpora/<collection>/<subset>.
func extractCorpusID(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	// Find the "corpora" segment and take the next two segments.
	for i, p := range parts {
		if p == "corpora" && i+2 < len(parts) {
			collection := parts[i+1]
			subset := parts[i+2]
			if collection == "" || subset == "" {
				return ""
			}
			return collection + "/" + subset
		}
	}
	return ""
}
