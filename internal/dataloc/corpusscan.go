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
	stdout, stderr, err := hostCommandRunner(context.Background(), host, corpusScanCommand())
	if err != nil {
		return nil, classifyScanError("scan corpus dir", host, stderr, err)
	}
	return parseCorpusScanOutput(stdout, host), nil
}

// corpusScanCommand builds the shell script that enumerates two-level corpus
// subdirectories under the base dir with sizes (GNU du -sb, then BSD du -sk
// fallback, then plain ls). A MISSING or unreadable base directory is reported
// via the scan sentinel and a non-zero exit so the caller treats it as
// "unknown" and skips pruning — an empty result must mean "the base dir is
// present and holds no corpora", never "the base dir is unmounted".
func corpusScanCommand() string {
	return fmt.Sprintf(`_corpus_dir="$HOME/%s"
if [ ! -d "$_corpus_dir" ]; then echo "%s $_corpus_dir" >&2; exit 3; fi
if [ ! -r "$_corpus_dir" ] || [ ! -x "$_corpus_dir" ]; then echo "%s $_corpus_dir" >&2; exit 3; fi
# POSIX: accumulate in the positional parameters. Arrays are a bash
# extension and this runs under /bin/sh, which is dash on Debian family.
set --
for _c in "$_corpus_dir"/*/; do
  [ -d "$_c" ] || continue
  for _s in "$_c"*/; do
    [ -d "$_s" ] && set -- "$@" "$_s"
  done
done
[ "$#" -eq 0 ] && exit 0
_out=$(du -sb "$@" 2>/dev/null)
if [ -n "$_out" ]; then
  printf '%%s\n' "$_out"
else
  _out=$(du -sk "$@" 2>/dev/null)
  if [ -n "$_out" ]; then
    printf '%%s\n' "$_out" | awk '{printf "%%d\t%%s\n", $1*1024, $2}'
  else
    printf '%%s\n' "$@"
  fi
fi`, CorpusBaseDir, scanBaseDirSentinel, scanBaseDirSentinel)
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
