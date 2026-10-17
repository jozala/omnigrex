#!/bin/sh
set -eu
image="${1:?supply the qualified development image digest}"
prefix=ghcr.io/jozala/omnigrex/development@sha256:
case "$image" in "$prefix"*) ;; *) exit 1 ;; esac
digest="${image#"$prefix"}"
test "${#digest}" -eq 64
case "$digest" in *[!0-9a-f]*) exit 1 ;; esac
printf 'version: 1\nimage: %s\nplatform: linux/amd64\nrequirements:\n  - hardware-isolation\n  - nested-containers\n  - tool-installation\n' "$image"
