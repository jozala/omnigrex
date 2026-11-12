#!/bin/sh
# Presentation only: publication is not qualification or production adoption.
set -eu
selected="${1:?selected production Pi required}"
candidate="${2:-}"
printf '## Build/publication output — not deployment approval\n\nExact-digest qualification and release readiness are still pending. Do not use this table as a deployment recommendation.\n\n'
# shellcheck disable=SC2016 # Markdown backticks.
printf 'Selected production Pi: `%s`\n\n' "$selected"
if [ -n "$candidate" ] && [ "$candidate" = "$selected" ]; then
  printf 'Newly published Pi digest matches the existing production selection; publication does not change selection.\n\n'
elif [ -n "$candidate" ]; then
  # shellcheck disable=SC2016 # Markdown backticks.
  printf 'Newly published Pi candidate — **Not selected for production**: `%s`\n\nChanging `.env` alone does not adopt this candidate or change the orchestrator approved resource inventory.\n\n' "$candidate"
fi
printf '| Built/published image (unqualified) | Digest |\n| --- | --- |\n'
shift 2
for name in orchestrator orchestrator-microsandbox development; do
  # shellcheck disable=SC2016 # Markdown backticks.
  printf '| %s | `%s` |\n' "$name" "$1"
  shift
done
# shellcheck disable=SC2016 # Markdown backticks.
printf '\nDevelopment image selected golden base: `%s`\n' "$selected"
