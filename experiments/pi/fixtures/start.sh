#!/bin/sh
set -eu
if test -n "${PROBE_LIVE_AUTH:-}"; then
    attempts=0
    until test -s "$PROBE_LIVE_AUTH"; do
        attempts=$((attempts + 1))
        test "$attempts" -lt 100
        sleep 0.1
    done
fi
node /opt/probe/fixtures/prepare.mjs
exec /opt/probe/node_modules/.bin/pi --mode rpc \
    --offline --no-approve --no-extensions --no-mcp --no-skills \
    --no-prompt-templates --no-themes --no-context-files \
    --session-dir /state --session-id "${PROBE_SESSION_ID:?}" \
    --provider "${PROBE_PROVIDER:-probe}" --model "${PROBE_MODEL:-cheap}" \
    --tools read,probe_echo --extension /opt/probe/fixtures/mcp-extension.mjs \
    --skill /workspace/.agents/skills/approved \
    --system-prompt 'You are an Omnigrex feasibility fixture. Follow the requested test exactly.' \
    --append-system-prompt 'Read the applicable AGENTS.md explicitly when asked; never load project configuration.'
