#!/bin/sh
set -eu
# Go owns its private go-build and testing.T.TempDir children and their cleanup.
# The native guest disk supports permission-sensitive tests; shared tool-data
# mounts can virtualize chmod and the Runtime Profile's /tmp is only 64 MiB.
if [ -d /opt/omnigrex/go-proxy ]; then
  export TMPDIR=/var/tmp GOTMPDIR=/var/tmp
fi
exec go test -count=1 "$@"
