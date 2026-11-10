#!/bin/sh
# Exercise later-source qualification of the real older selected image without
# inventing a published R2 or changing production selection. Discard the receipt.
set -eu
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT HUP INT TERM
go run ./scripts/pi-bundle record | jq '{version:1,platform,image,source_commit,resources}' > "$work/candidate.json"
OMNIGREX_PI_CANDIDATE_FILE="$work/candidate.json" \
OMNIGREX_PI_BUNDLE_OUTPUT="$work/result" \
sh scripts/qualify-pi-bundle.sh
jq -S . "$work/candidate.json" > "$work/expected.json"
jq -S . "$work/result/candidate.json" > "$work/actual.json"
cmp "$work/expected.json" "$work/actual.json"
