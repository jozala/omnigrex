#!/bin/sh
# Validate dispatch intent before any job receives publication authority.
set -eu
test "${GITHUB_EVENT_NAME:-}" = workflow_dispatch
test "${GITHUB_REF:-}" = refs/heads/main
case "${PI_MODE:-}" in
  build-candidate) test -z "${PI_CANDIDATE:-}" ;;
  qualify-candidate) test -n "${PI_CANDIDATE:-}" ;;
  *) echo 'Choose build-candidate (no pi_candidate) or qualify-candidate (captured pi_candidate required)' >&2; exit 1 ;;
esac
