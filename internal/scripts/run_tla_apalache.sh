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
for cfg_script in "$SPECDIR"/*.cfg.script; do
    [ -e "$cfg_script" ] || continue
    cfg_target="${cfg_script%.cfg.script}.cfg"
    "$cfg_script" > "$cfg_target"
done

found=0
for spec in "$SPECDIR"/*.tla; do
    [ -e "$spec" ] || continue
    found=1
    if grep -q -- "--algorithm" "$spec"; then
        "$JAVA_BIN" -cp "$TLA_JAR" pcal.trans "$spec"
    fi
    cfg_file="${spec%.tla}.cfg"
    if [ ! -f "$cfg_file" ]; then
        echo "Skipping $spec (no matching .cfg)" >&2
        continue
    fi

    # Skip PlusCal specs (BEGIN TRANSLATION marker) - Apalache needs type annotations
    # that would be overwritten when PlusCal retranslates
    if grep -q "BEGIN TRANSLATION" "$spec"; then
        echo "Skipping $spec (PlusCal specs require type annotations for Apalache)" >&2
        continue
    fi

    inv_args=""
    while IFS= read -r line; do
        case "$line" in
            INVARIANT\ *)
                name=$(echo "$line" | awk '{print $2}')
                inv_args="$inv_args --inv=${name}"
                ;;
        esac
    done < "$cfg_file"

    apalache-mc check --config="$cfg_file" $inv_args "$spec"
done

if [ "$found" -eq 0 ]; then
    echo "No .tla specs under $SPECDIR" >&2
fi
