#!/bin/sh
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
SPECDIR="$ROOT/docs/specs"
TLA_JAR="${TLA_JAR:-$HOME/lib/tla2tools.jar}"
JAVA_BIN="${JAVA_BIN:-java}"

if [ ! -d "$SPECDIR" ]; then
    echo "Spec directory $SPECDIR not found" >&2
    exit 1
fi

# Generate configs from any *.cfg.script helpers
CFG_NEEDS_CHECK=0

for cfg_script in "$SPECDIR"/*.cfg.script; do
    [ -e "$cfg_script" ] || continue
    cfg_target="${cfg_script%.cfg.script}.cfg"
    cfg_tmp="${cfg_target}.tmp"
    "$cfg_script" > "$cfg_tmp"
    if git -C "$ROOT" ls-files --error-unmatch "$cfg_target" >/dev/null 2>&1; then
        if ! cmp -s "$cfg_target" "$cfg_tmp"; then
            CFG_NEEDS_CHECK=1
        fi
    fi
    mv "$cfg_tmp" "$cfg_target"
done

if [ "$CFG_NEEDS_CHECK" -eq 1 ]; then
    echo "Tracked .cfg files under $SPECDIR were regenerated from .cfg.script" >&2
    echo "Commit updated .cfg files or ignore them in .gitignore." >&2
    exit 2
fi

found=0
for spec in "$SPECDIR"/*.tla; do
    [ -e "$spec" ] || continue
    found=1
    if grep -q -- "--algorithm" "$spec"; then
        "$JAVA_BIN" -cp "$TLA_JAR" pcal.trans "$spec"
    fi
    cfg_file="${spec%.tla}.cfg"
    if [ -f "$cfg_file" ]; then
        "$JAVA_BIN" -cp "$TLA_JAR" tlc2.TLC -deadlock -config "$cfg_file" "$spec"
    else
        echo "Skipping $spec (no matching .cfg)" >&2
    fi
done

if [ "$found" -eq 0 ]; then
    echo "No .tla specs under $SPECDIR" >&2
fi
