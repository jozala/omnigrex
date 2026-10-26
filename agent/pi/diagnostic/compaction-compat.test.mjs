import assert from 'node:assert/strict';
import { test } from 'node:test';
import { compactionCompatibilityExtension } from '../compaction-compat-extension.mjs';

const entry = milliseconds => ({ type: 'compaction', timestamp: new Date(milliseconds).toISOString() });

function fixture(entries, clock) {
  let compact;
  compactionCompatibilityExtension({ on(name, handler) {
    assert.equal(name, 'session_compact');
    compact = handler;
  } }, clock);
  const ctx = { sessionManager: { getBranch: () => entries } };
  return () => compact({ compactionEntry: entries[0] }, ctx);
}

test('same-millisecond continuation waits until strictly after compaction without changing history', async () => {
  const entries = [entry(1000)];
  const before = JSON.stringify(entries);
  let now = 1000, waits = 0;
  await fixture(entries, { now: () => now, wait: async () => {
    waits++;
    if (waits === 2) now++;
  } })();
  assert.equal(waits, 2, 'an early timer must not release the timestamp boundary');
  assert.ok(now > Date.parse(entries[0].timestamp));
  assert.equal(JSON.stringify(entries), before);
});

test('already-newer clock requires no scheduling delay', async () => {
  await fixture([entry(1000)], { now: () => 1001, wait: () => assert.fail('unnecessary wait') })();
});

test('repeated summaries use the latest branch compaction rather than the stale event entry', async () => {
  const entries = [entry(1000), { type: 'message' }, entry(2000), { type: 'message' }];
  let now = 2000, waits = 0;
  await fixture(entries, { now: () => now, wait: async () => { waits++; now++; } })();
  assert.equal(waits, 1);
});

for (const now of [1000, 900]) {
  test(`clock that stays at ${now} cannot block continuation indefinitely`, async () => {
    let waits = 0;
    await fixture([entry(1000)], { now: () => now, wait: async () => { waits++; } })();
    assert.ok(waits > 0 && waits <= 32, 'compatibility wait must have a fixed bound');
  });
}

test('absent or invalid compaction timestamps do not delay continuation', async () => {
  for (const entries of [[], [{ type: 'compaction', timestamp: 'invalid' }]]) {
    await fixture(entries, { now: () => 1000, wait: () => assert.fail('invalid boundary waited') })();
  }
});
