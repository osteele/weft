#!/usr/bin/env bash

set -euo pipefail

required_version="3.5.3"

if ! command -v allium >/dev/null 2>&1; then
    echo "error: allium ${required_version} is required; see specs/README.md" >&2
    exit 2
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "error: jq is required to inspect Allium diagnostics" >&2
    exit 2
fi

installed_version="$(allium --version | awk '{print $2}')"
if [[ "${installed_version}" != "${required_version}" ]]; then
    echo "error: allium ${required_version} is required (found ${installed_version})" >&2
    exit 2
fi

diagnostics="$(mktemp "${TMPDIR:-/tmp}/weft-allium.XXXXXX")"
trap 'rm -f "${diagnostics}"' EXIT

set +e
allium check specs >"${diagnostics}"
allium_status=$?
set -e

if (( allium_status > 1 )); then
    echo "error: allium could not check specs" >&2
    exit "${allium_status}"
fi

read -r spec_count error_count warning_count info_count unexpected_warning_count < <(
    jq -sr '
        [length,
         ([.[].diagnostics[] | select(.severity == "error")] | length),
         ([.[].diagnostics[] | select(.severity == "warning")] | length),
         ([.[].diagnostics[] | select(.severity == "info")] | length),
         ([.[].diagnostics[]
           | select(.severity == "warning")
           | select(.code != "allium.definition.unused"
                    and .code != "allium.entity.unused"
                    and .code != "allium.externalEntity.missingSourceHint")]
          | length)]
        | @tsv
    ' "${diagnostics}"
)

echo "Allium ${required_version}: ${spec_count} specs, ${error_count} errors, ${warning_count} warnings, ${info_count} infos"

if (( error_count > 0 )); then
    jq -sr -r '
        .[].diagnostics[]
        | select(.severity == "error")
        | "\(.location.file):\(.location.line): \(.code): \(.message)"
    ' "${diagnostics}" >&2
    exit 1
fi

if (( unexpected_warning_count > 0 )); then
    echo "error: unexpected Allium warning diagnostics:" >&2
    jq -sr -r '
        .[].diagnostics[]
        | select(.severity == "warning")
        | select(.code != "allium.definition.unused"
                 and .code != "allium.entity.unused"
                 and .code != "allium.externalEntity.missingSourceHint")
        | "\(.location.file):\(.location.line): \(.code): \(.message)"
    ' "${diagnostics}" >&2
    exit 1
fi

if (( warning_count > 0 )); then
    echo "Non-blocking warning summary:"
    jq -sr -r '
        [.[].diagnostics[] | select(.severity == "warning")]
        | group_by(.code)
        | map({code: .[0].code, count: length})
        | sort_by(-.count, .code)[]
        | "  \(.count) \(.code)"
    ' "${diagnostics}"
fi
