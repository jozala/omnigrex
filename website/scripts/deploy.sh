#!/bin/sh
set -eu

: "${1:?pass the downloaded static artifact directory}"
: "${RUNNER_TEMP:?}"
: "${WEBSITE_SSH_HOST:?}"
: "${WEBSITE_SSH_USER:?}"
: "${WEBSITE_SSH_KEY:?}"
: "${WEBSITE_SSH_KNOWN_HOSTS:?}"

umask 077
ssh_directory=$(mktemp -d "$RUNNER_TEMP/website-ssh.XXXXXX")
key="$ssh_directory/id_ed25519"
known_hosts="$ssh_directory/known_hosts"
trap 'rm -f "$key" "$known_hosts"; rmdir "$ssh_directory"' EXIT
printf '%s\n' "$WEBSITE_SSH_KEY" > "$key"
printf '%s\n' "$WEBSITE_SSH_KNOWN_HOSTS" > "$known_hosts"

transport="ssh -F /dev/null -i \"$key\" -p \"${WEBSITE_SSH_PORT:-22}\" -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none -o StrictHostKeyChecking=yes -o UserKnownHostsFile=\"$known_hosts\""

# The server's forced rrsync command selects and confines the live document root.
# Keep arguments visible to rrsync, which rejects --protect-args.
rsync -az --delete --no-owner --no-group \
  -e "$transport" -- "${1%/}/" \
  "$WEBSITE_SSH_USER@$WEBSITE_SSH_HOST:./"
