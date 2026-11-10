#!/bin/sh
# Explicit pre-adoption runtime qualification, never production admission.
set -eu
: "${OMNIGREX_PI_CANDIDATE_FILE:?provide the captured candidate record}"
: "${OMNIGREX_PI_BUNDLE_OUTPUT:?provide an empty output directory}"
test ! -e "$OMNIGREX_PI_BUNDLE_OUTPUT"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT HUP INT TERM
go run ./scripts/pi-bundle validate-candidate > "$work/candidate.json"
source_image="$(go run ./scripts/pi-bundle image)"
target_image="$(jq -er .image "$work/candidate.json")"
go run ./scripts/pi-bundle profile > "$work/source-profile.json"
docker pull --platform linux/amd64 "$source_image"
docker pull --platform linux/amd64 "$target_image"
version() {
  docker run --rm --network none --entrypoint /usr/local/bin/node "$1" -p "require('/opt/omnigrex/pi/package.json').dependencies['@earendil-works/pi-coding-agent']"
}
OMNIGREX_PI_UPGRADE_SOURCE_IMAGE="$source_image" \
OMNIGREX_PI_UPGRADE_TARGET_IMAGE="$target_image" \
OMNIGREX_PI_UPGRADE_SOURCE_CONTRACT_FILE="$work/source-profile.json" \
OMNIGREX_PI_UPGRADE_SOURCE_VERSION="$(version "$source_image")" \
OMNIGREX_PI_UPGRADE_TARGET_VERSION="$(version "$target_image")" \
OMNIGREX_PI_UPGRADE_SELECTED_SOURCE=1 \
OMNIGREX_PI_BUNDLE_CANDIDATE_FILE="$work/candidate.json" \
sh scripts/test-pi-upgrade.sh
# Success proves this exact image/inventory passed the resource, protocol,
# continuation/replay and fresh-Session checks in the integration-only fixture.
mkdir "$OMNIGREX_PI_BUNDLE_OUTPUT"
cp "$work/candidate.json" "$OMNIGREX_PI_BUNDLE_OUTPUT/candidate.json"
