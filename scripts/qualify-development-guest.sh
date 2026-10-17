#!/bin/sh
set -eu
turn="$1"
test "$(id -u):$(id -g)" = 10001:10001
docker info
docker compose version
docker buildx version
case "$GOPROXY" in file:///opt/omnigrex/go-proxy,*) ;; *) exit 1 ;; esac
# Keep compilation inside the normal Runtime Profile's bounded PID budget.
export GOMAXPROCS=2 GOFLAGS='-p=1 -modcacherw'
# Explicit tasks need discovery; ordinary Pi execution keeps it disabled.
project_mise() {
  env -u MISE_IGNORED_CONFIG_PATHS -u MISE_OVERRIDE_CONFIG_FILENAMES \
    -u MISE_OVERRIDE_TOOL_VERSIONS_FILENAMES /usr/local/bin/mise "$@"
}
if [ "$turn" = 1 ]; then
  sh scripts/qualify-development-compose.sh
  GOPROXY=file:///opt/omnigrex/go-proxy go mod download
  project_mise run check
else
  # A fresh VM can use the same Participant's cache without any module source.
  GOPROXY=off go mod download
  # Fail early on the concurrent-container capacity and historical-contract
  # boundaries before running the complete database-backed suite.
  if sh scripts/go-test.sh -p 1 -race -tags=integration -count=1 -timeout 5m \
    -run '^(TestExecutionOrphanCleanupPreservesLegitimateProfileReservation|TestRepositoryRuntimeContractIsCommittedAtomicallyAndImmutable)$' ./internal/store && \
    project_mise run test-integration; then
    :
  else
    status=$?
    for name in pids.current pids.max pids.events; do
      printf 'workload %s:\n' "$name" >&2
      cat "/sys/fs/cgroup/microsandbox-workload/$name" >&2 || true
    done
    exit "$status"
  fi
  docker run --detach --name development-background \
    docker.io/library/busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092 sleep 600
fi
