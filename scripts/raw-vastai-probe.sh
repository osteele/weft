#!/usr/bin/env bash
# Non-weft probe of Vast.ai launch path.
#
# Bypasses weft entirely: searches a cheap offer, creates an instance with
# a tiny inline onstart that PUTs a marker to a presigned R2 URL. Polls the
# marker for up to ~5 minutes. Reports outcome. Always destroys the
# instance.
#
# Distinguishes "weft is broken" from "Vast/network is broken." If this
# script's onstart marker appears, raw Vast launches reach R2 fine — any
# weft launch failures with no probe marker are weft's responsibility. If
# even raw vastai's marker doesn't appear, the failure is provider-side or
# network-side and weft can't fix it.
#
# Usage:
#   ./scripts/raw-vastai-probe.sh             # pick a cheap offer, run probe
#   ./scripts/raw-vastai-probe.sh <offer_id>  # use a specific offer
set -euo pipefail

cd "$(dirname "$0")/.."

# Pull the same R2 credentials weft uses.
R2_ACCESS_KEY_ID=$(awk -F'"' '/access_key_id/{print $2; exit}' ~/.config/weft/config.toml)
R2_SECRET=$(awk -F'"' '/secret_access_key/{print $2; exit}' ~/.config/weft/config.toml)
R2_ACCOUNT=$(awk -F'"' '/account_id/{print $2; exit}' ~/.config/weft/config.toml)
R2_BUCKET=$(awk -F'"' '/^[[:space:]]*bucket/{print $2; exit}' ~/.config/weft/config.toml)

PROBE_TS=$(date -u +%FT%TZ)
PROBE_KEY="probes/raw-vastai-$(date -u +%Y%m%d-%H%M%S)-$$"

# Generate a presigned PUT URL via a tiny Go helper that links against
# weft's r2 package. The URL is what the container will PUT to.
PROBE_URL=$(mkdir -p .probe-tmp && cat > .probe-tmp/main.go <<GO
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/osteele/weft/internal/r2"
)

func main() {
	c, err := r2.New(r2.Config{
		AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("R2_BUCKET"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	url, err := c.PresignPutURL(context.Background(), os.Getenv("PROBE_KEY"), time.Hour)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(url)
}
GO
R2_ACCOUNT_ID="$R2_ACCOUNT" R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" R2_SECRET_ACCESS_KEY="$R2_SECRET" R2_BUCKET="$R2_BUCKET" PROBE_KEY="$PROBE_KEY" go run ./.probe-tmp 2>/dev/null)
rm -rf .probe-tmp

if [ -z "$PROBE_URL" ]; then
	echo "failed to generate presigned URL" >&2
	exit 1
fi

echo "probe key: $PROBE_KEY"

# Pick an offer if none was specified. Cheap, on-demand, RTX_3060-ish.
if [ -z "${1:-}" ]; then
	OFFER=$(vastai search offers 'reliability >= 0.99 verified=true rented=false dlperf>15 dlperf<60 inet_down>200 cuda_max_good>=12.4 disk_space>=30 cpu_cores>=4' -o 'dph_total' --raw 2>/dev/null \
		| python3 -c 'import sys,json; o=json.load(sys.stdin); print(o[0]["id"])')
else
	OFFER=$1
fi
echo "offer: $OFFER"

# Trivial onstart — no apt, no rclone, no weft. Just curl the probe URL,
# then sleep so the container survives long enough to be observed.
ONSTART=$(cat <<EOF
(command -v curl >/dev/null && curl -fsS -m 10 -X PUT --data 'raw-vastai-probe-onstart-${PROBE_TS}' "$PROBE_URL" >/dev/null 2>&1) || \
(command -v wget >/dev/null && wget -q --method=PUT --body-data='raw-vastai-probe-onstart-${PROBE_TS}' -O /dev/null "$PROBE_URL") || \
echo no-curl-no-wget >&2
sleep 600
EOF
)

echo "creating instance..."
CREATE_OUT=$(vastai create instance "$OFFER" \
	--image nvidia/cuda:12.4.1-runtime-ubuntu22.04 \
	--disk 30 \
	--label weft/raw-probe \
	--onstart-cmd "$ONSTART" 2>&1)
INST_ID=$(echo "$CREATE_OUT" | grep -oE 'new_contract.*[0-9]+' | grep -oE '[0-9]+' | head -1)
if [ -z "$INST_ID" ]; then
	echo "$CREATE_OUT" | tail -10
	exit 1
fi
echo "instance: $INST_ID"

cleanup() { echo "destroying $INST_ID..."; printf 'y\n' | vastai destroy instance "$INST_ID" 2>&1 | tail -2; }
trap cleanup EXIT

# Poll for the probe marker.
echo "polling R2 for probe marker (up to ~6 min)..."
T0=$(date +%s)
while [ $(($(date +%s) - T0)) -lt 360 ]; do
	BODY=$(AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$R2_SECRET" AWS_DEFAULT_REGION=auto \
		aws --endpoint-url "https://${R2_ACCOUNT}.r2.cloudflarestorage.com" \
		s3 cp "s3://${R2_BUCKET}/${PROBE_KEY}" - 2>/dev/null) || true
	if [ -n "$BODY" ]; then
		echo "PROBE MARKER ARRIVED at t+$(($(date +%s) - T0))s: $BODY"
		# Cleanup probe object
		AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$R2_SECRET" AWS_DEFAULT_REGION=auto \
			aws --endpoint-url "https://${R2_ACCOUNT}.r2.cloudflarestorage.com" \
			s3 rm "s3://${R2_BUCKET}/${PROBE_KEY}" >/dev/null 2>&1 || true
		exit 0
	fi
	sleep 15
done

echo "TIMEOUT — no probe marker after 6 minutes"
echo "Vast says:"
vastai show instance "$INST_ID" --raw 2>/dev/null | python3 -c 'import sys,json; d=json.load(sys.stdin); [print(f"  {k}: {d.get(k)}") for k in ["actual_status","status_msg","public_ipaddr","ssh_port","duration"]]' || true
exit 2
