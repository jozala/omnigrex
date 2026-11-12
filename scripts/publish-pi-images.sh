#!/bin/sh
set -eu
: "${OMNIGREX_RELEASE_COMMIT:?set the full release commit}"
: "${GITHUB_STEP_SUMMARY:?publication runs in release CI}"
golden_image="$(go run ./scripts/pi-bundle image)"
docker pull --platform linux/amd64 "$golden_image"
for name in pi orchestrator orchestrator-microsandbox development; do
  set --
  case "$name" in
    pi) file=agent/pi/Dockerfile; target=golden ;;
    orchestrator) file=Dockerfile; target=runtime ;;
    orchestrator-microsandbox) file=Dockerfile.microsandbox; target=runtime ;;
    development)
      file=agent/environments/omnigrex/Dockerfile; target=development
      test -n "$golden_image"
      set -- --build-arg "OMNIGREX_GOLDEN_IMAGE=$golden_image" \
        --label "org.opencontainers.image.base.name=$golden_image"
      ;;
  esac
  tag="ghcr.io/jozala/omnigrex/$name:sha-$OMNIGREX_RELEASE_COMMIT"
  docker build --platform linux/amd64 --file "$file" --target "$target" \
    --build-arg "VERSION=$OMNIGREX_RELEASE_COMMIT" \
    "$@" \
    --label org.opencontainers.image.source=https://github.com/jozala/omnigrex --tag "$tag" .
  test "$(docker image inspect "$tag" --format '{{.Config.User}}')" = 10001:10001
  test "$(docker image inspect "$tag" --format '{{.Os}}/{{.Architecture}}')" = linux/amd64
  test "$(docker image inspect "$tag" --format '{{index .Config.Labels "org.opencontainers.image.source"}}')" = https://github.com/jozala/omnigrex
  docker push "$tag"
  digest="$(docker image inspect "$tag" --format '{{index .RepoDigests 0}}')"
  case "$digest" in "ghcr.io/jozala/omnigrex/$name@sha256:"*) ;; *) exit 1 ;; esac
  docker pull "$digest"
  case "$name" in
    pi) candidate_image="$digest" ;;
    orchestrator) orchestrator_image="$digest" ;;
    orchestrator-microsandbox) microsandbox_image="$digest" ;;
    development) development_image="$digest" ;;
  esac
  # Publication does not adopt the newly built Pi candidate.
  if [ "$name" = pi ]; then
    if [ -n "${GITHUB_OUTPUT:-}" ]; then printf 'pi_candidate_image=%s\n' "$digest" >> "$GITHUB_OUTPUT"; fi
    digest="$golden_image"
  fi
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    key="$(printf '%s' "$name" | tr '-' '_')"
    printf '%s_image=%s\n' "$key" "$digest" >> "$GITHUB_OUTPUT"
  fi
done
sh scripts/write-publication-summary.sh "$golden_image" "${candidate_image:-}" \
  "$orchestrator_image" "$microsandbox_image" "$development_image" >> "$GITHUB_STEP_SUMMARY"
