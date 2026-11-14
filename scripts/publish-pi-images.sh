#!/bin/sh
set -eu
: "${OMNIGREX_RELEASE_COMMIT:?set the full release commit}"
: "${GITHUB_STEP_SUMMARY:?publication runs in release CI}"
case "${1:-product}" in
  product)
    names='orchestrator orchestrator-microsandbox development'
    golden_image="$(go run ./scripts/pi-bundle image)"
    docker pull --platform linux/amd64 "$golden_image"
    ;;
  candidate)
    # Also guard direct invocation, before any Docker operation.
    sh scripts/validate-pi-dispatch.sh
    test "$PI_MODE" = build-candidate
    names=pi
    ;;
  *) echo 'Expected product or candidate publication mode' >&2; exit 1 ;;
esac
for name in $names; do
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
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    case "$name" in
      pi) key=pi_candidate ;;
      *) key="$(printf '%s' "$name" | tr '-' '_')" ;;
    esac
    printf '%s_image=%s\n' "$key" "$digest" >> "$GITHUB_OUTPUT"
  fi
done
if [ "$names" = pi ]; then
  # shellcheck disable=SC2016 # Markdown backticks.
  printf '## Unselected Pi candidate — not qualification or adoption\n\nPublished: `%s`\n\nUse the captured candidate record in a separate qualification dispatch. Selection and repository pins are unchanged.\n' "$candidate_image" >> "$GITHUB_STEP_SUMMARY"
else
  if [ -n "${GITHUB_OUTPUT:-}" ]; then printf 'pi_image=%s\n' "$golden_image" >> "$GITHUB_OUTPUT"; fi
  sh scripts/write-publication-summary.sh "$golden_image" "" \
    "$orchestrator_image" "$microsandbox_image" "$development_image" >> "$GITHUB_STEP_SUMMARY"
fi
