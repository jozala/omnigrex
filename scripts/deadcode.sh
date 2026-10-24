#!/bin/sh
set -eu
# Shared local/CI gate; keep the explicit formatting-method exceptions exact.
if [ "$#" -eq 0 ]; then set -- deadcode; fi
if ! output="$("$@" ./...)"; then
  exit 1
fi

unexpected="$(printf '%s\n' "$output" | grep -Ev '^(internal/agentturn/execution_worker\.go:[0-9]+:[0-9]+: unreachable func: (ExecutionWorkerConfig|ExecutionWorker)\.(String|GoString)|internal/agentturn/outcome_reconciler\.go:[0-9]+:[0-9]+: unreachable func: OutcomeReconcilerConfig\.(String|GoString))$' || true)"
if [ -n "$unexpected" ]; then
  printf '%s\n' "$unexpected"
  exit 1
fi
