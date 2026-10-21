#!/bin/sh
set -eu
: "${OMNIGREX_DEVELOPMENT_IMAGE:?set the exact published development image digest}"
: "${OMNIGREX_MICROSANDBOX_KVM_GID:?set the approved Linux/amd64 Docker host KVM group}"
case "$OMNIGREX_DEVELOPMENT_IMAGE" in *@sha256:*) ;; *) exit 1 ;; esac
sh scripts/docker-build.sh --platform linux/amd64 --file Dockerfile.microsandbox \
  --build-context development-docs=./docs \
  --target development-qualification --tag omnigrex/development-qualification:dev .
docker run --rm --platform linux/amd64 --device /dev/kvm --group-add "$OMNIGREX_MICROSANDBOX_KVM_GID" \
  --memory 6g --memory-swap 6g --cpus 2 --pids-limit 2048 \
  --env "OMNIGREX_DEVELOPMENT_IMAGE=$OMNIGREX_DEVELOPMENT_IMAGE" \
  omnigrex/development-qualification:dev \
  -test.v -test.run '^TestMicrosandboxDevelopmentEnvironment$' -test.timeout 55m
