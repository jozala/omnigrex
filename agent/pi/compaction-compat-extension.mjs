import { setTimeout as delay } from 'node:timers/promises';

// Pi 1.4.0 compares assistant and compaction timestamps with <=. Its awaited
// post-persistence hook must finish in a later millisecond before native retry.
// Remove when a qualified SDK uses Session ordering instead of wall-clock time.
export function compactionCompatibilityExtension(pi, {
  now = () => Date.now(), wait = () => delay(1),
} = {}) {
  pi.on('session_compact', async (_event, ctx) => {
    // The SDK event can refer to an older entry when summary text repeats.
    const latest = ctx.sessionManager.getBranch().findLast(entry => entry.type === 'compaction');
    const boundary = Date.parse(latest?.timestamp);
    if (!Number.isFinite(boundary)) return;
    // Bound scheduling attempts even if the wall clock freezes or rolls back.
    // On exhaustion, retain native provider failure/settlement handling rather
    // than adding retries, mutating timestamps, or blocking cancellation forever.
    for (let attempt = 0; attempt < 32 && now() <= boundary; attempt++) await wait();
  });
}
