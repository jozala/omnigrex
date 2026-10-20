#!/bin/sh
# Verify a built Omnigrex Docker controller image without pinning package revisions.
# Usage: sh scripts/check-orchestrator-image.sh IMAGE
# Requires Docker on the daemon host. The image is exercised as its default
# non-root user; no fixed package count or revision is asserted.
set -eu
image="${1:?supply the controller image reference}"
user="$(docker image inspect "$image" --format '{{.Config.User}}')"
test "$user" = 10001:10001
docker run --rm --entrypoint /bin/sh "$image" -ec '
  set -eu
  test "$(id -u):$(id -g)" = 10001:10001
  command -v bash
  command -v curl
  command -v git
  command -v jq
  command -v gcc
  command -v ssh
  command -v rg
  bash --version
  curl --version
  git --version
  jq --version
  gcc --version
  ssh -V 2>&1
  rg --version
  test -s /etc/ssl/certs/ca-certificates.crt
  test -x "$(git --exec-path)/git-remote-https"
  test -r /usr/share/omnigrex/apk-packages.txt
  test -s /usr/share/omnigrex/apk-packages.txt
  for package in bash ca-certificates curl git jq gcc musl-dev openssh-client-default ripgrep; do
    grep -q "^${package}-" /usr/share/omnigrex/apk-packages.txt
  done
  apk info -v | sort > /tmp/actual-packages.txt
  diff -u /usr/share/omnigrex/apk-packages.txt /tmp/actual-packages.txt
  printf "#include <stdio.h>\nint main(void) { return 0; }\n" > /tmp/controller-check.c
  gcc /tmp/controller-check.c -o /tmp/controller-check
  /tmp/controller-check
'
printf 'PASS controller image %s\n' "$image"
