#!/bin/sh
set -eu
: "${OMNIGREX_RELEASE_COMMIT:?set the full release commit}"
: "${GITHUB_STEP_SUMMARY:?publication runs in release CI}"
printf '## Published Omnigrex images\n\nCommit: %s\n\n| Image | Tag | Digest |\n| --- | --- | --- |\n' "$OMNIGREX_RELEASE_COMMIT" >> "$GITHUB_STEP_SUMMARY"
for name in pi orchestrator orchestrator-microsandbox; do
  case "$name" in
    pi) file=agent/pi/Dockerfile; target=golden ;;
    orchestrator) file=Dockerfile; target=runtime ;;
    orchestrator-microsandbox) file=Dockerfile.microsandbox; target=runtime ;;
  esac
  tag="ghcr.io/jozala/omnigrex/$name:sha-$OMNIGREX_RELEASE_COMMIT"
  docker build --platform linux/amd64 --file "$file" --target "$target" \
    --build-arg "VERSION=$OMNIGREX_RELEASE_COMMIT" \
    --label org.opencontainers.image.source=https://github.com/jozala/omnigrex --tag "$tag" .
  test "$(docker image inspect "$tag" --format '{{.Config.User}}')" = 10001:10001
  test "$(docker image inspect "$tag" --format '{{.Os}}/{{.Architecture}}')" = linux/amd64
  test "$(docker image inspect "$tag" --format '{{index .Config.Labels "org.opencontainers.image.source"}}')" = https://github.com/jozala/omnigrex
  docker push "$tag"
  digest="$(docker image inspect "$tag" --format '{{index .RepoDigests 0}}')"
  case "$digest" in "ghcr.io/jozala/omnigrex/$name@sha256:"*) ;; *) exit 1 ;; esac
  docker pull "$digest"
  printf '| %s | `%s` | `%s` |\n' "$name" "$tag" "$digest" >> "$GITHUB_STEP_SUMMARY"
done
