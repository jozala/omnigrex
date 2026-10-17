#!/bin/sh
set -eu
: "${OMNIGREX_RELEASE_COMMIT:?set the full release commit}"
: "${GITHUB_STEP_SUMMARY:?publication runs in release CI}"
printf '## Published Omnigrex images\n\nCommit: %s\n\n| Image | Tag | Digest |\n| --- | --- | --- |\n' "$OMNIGREX_RELEASE_COMMIT" >> "$GITHUB_STEP_SUMMARY"
golden_image=
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
  # shellcheck disable=SC2016 # Backticks delimit Markdown, not shell commands.
  printf '| %s | `%s` | `%s` |\n' "$name" "$tag" "$digest" >> "$GITHUB_STEP_SUMMARY"
  if [ "$name" = pi ]; then golden_image="$digest"; fi
  if [ "$name" = development ] && [ -n "${GITHUB_OUTPUT:-}" ]; then
    printf 'development_image=%s\n' "$digest" >> "$GITHUB_OUTPUT"
  fi
done
# shellcheck disable=SC2016 # Backticks delimit Markdown, not shell commands.
printf '\nDevelopment image golden base: `%s`\n' "$golden_image" >> "$GITHUB_STEP_SUMMARY"
