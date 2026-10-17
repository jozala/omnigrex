#!/bin/sh
set -eu
project=omnigrex-development-qualification
compose() {
  docker compose --project-name "$project" \
    --file agent/environments/omnigrex/compose-qualification.yaml "$@"
}
cleanup() {
  compose down --volumes --remove-orphans --timeout 10
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
compose up --wait --wait-timeout 60 web
compose run --rm --no-deps client
cleanup
trap - EXIT HUP INT TERM
containers="$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project")"
test -z "$containers"
networks="$(docker network ls --quiet --filter "label=com.docker.compose.project=$project")"
test -z "$networks"
volumes="$(docker volume ls --quiet --filter "label=com.docker.compose.project=$project")"
test -z "$volumes"
