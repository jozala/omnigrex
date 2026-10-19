import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, rm, symlink } from 'node:fs/promises';
import { join } from 'node:path';
import { openDurableSession } from './session-state.mjs';

const identity = { agentSessionId: '11111111-1111-4111-8111-111111111111', runtimeSessionId: '11111111-1111-4111-8111-111111111111', mustExist: false };

test('creation persists exact identity before a conversation and recovers after response loss', async () => {
  const root = await mkdtemp('/tmp/pi-state-test-');
  try {
    const first = await openDurableSession(identity, root);
    assert.equal(first.disposition, 'created');
    assert.equal(first.manager.getSessionId(), identity.runtimeSessionId);
    const bytes = await readFile(first.manager.getSessionFile(), 'utf8');
    assert.equal(JSON.parse(bytes).id, identity.runtimeSessionId);
    const reopened = await openDurableSession(identity, root);
    assert.equal(reopened.disposition, 'recovered');
    assert.equal(reopened.manager.getSessionFile(), first.manager.getSessionFile());
    assert.equal(await readFile(first.manager.getSessionFile(), 'utf8'), bytes);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('activated missing state cannot become an empty Session', async () => {
  const root = await mkdtemp('/tmp/pi-state-test-');
  try { await assert.rejects(openDurableSession({ ...identity, mustExist: true }, root)); }
  finally { await rm(root, { recursive: true, force: true }); }
});

test('reopening rejects malformed context without Pi normalization or file changes', async () => {
  const fixtures = {
    'missing user content': [
      { type: 'message', id: '11111111', parentId: null, message: { role: 'user', timestamp: 1 } },
    ],
    'unknown compaction retained entry': [
      { type: 'message', id: '11111111', parentId: null, message: { role: 'user', content: 'prior context', timestamp: 1 } },
      { type: 'compaction', id: '22222222', parentId: '11111111', summary: 'summary', firstKeptEntryId: 'nonexistent', tokensBefore: 1 },
    ],
    'compaction retained entry on another branch': [
      { type: 'message', id: '11111111', parentId: null, message: { role: 'user', content: 'root', timestamp: 1 } },
      { type: 'message', id: '22222222', parentId: '11111111', message: { role: 'user', content: 'abandoned', timestamp: 1 } },
      { type: 'compaction', id: '33333333', parentId: '11111111', summary: 'summary', firstKeptEntryId: '22222222', tokensBefore: 1 },
    ],
    'missing content-edit target': [
      { type: 'context_edit', id: '11111111', parentId: null, targetId: 'nonexistent', replacement: null },
    ],
    'malformed tool result': [
      { type: 'message', id: '11111111', parentId: null, message: { role: 'toolResult', content: null, timestamp: 1 } },
    ],
  };
  for (const [name, entries] of Object.entries(fixtures)) {
    const root = await mkdtemp('/tmp/pi-state-test-');
    try {
      const { manager } = await openDurableSession(identity, root);
      const file = manager.getSessionFile();
      const header = await readFile(file, 'utf8');
      const bytes = header + entries.map(entry => JSON.stringify({ timestamp: '2026-10-09T12:00:00Z', ...entry }) + '\n').join('');
      await writeFile(file, bytes);
      await assert.rejects(openDurableSession({ ...identity, mustExist: true }, root), name);
      assert.equal(await readFile(file, 'utf8'), bytes, 'corrupt context bytes were changed');
    } finally { await rm(root, { recursive: true, force: true }); }
  }
});

test('max and xhigh thinking levels reopen with exact values', async () => {
  for (const level of ['max', 'xhigh']) {
    const root = await mkdtemp('/tmp/pi-state-test-');
    try {
      const { manager } = await openDurableSession(identity, root);
      const file = manager.getSessionFile();
      const header = await readFile(file, 'utf8');
      const entries = [
        { type: 'message', id: '11111111', parentId: null, message: { role: 'user', content: 'prior context', timestamp: 1 } },
        { type: 'thinking_level_change', id: '22222222', parentId: '11111111', thinkingLevel: level },
      ];
      const bytes = header + entries.map(entry => JSON.stringify({ timestamp: '2026-10-09T12:00:00Z', ...entry }) + '\n').join('');
      await writeFile(file, bytes);
      const reopened = await openDurableSession({ ...identity, mustExist: true }, root);
      assert.equal(reopened.manager.getSessionId(), identity.runtimeSessionId);
      const retained = await readFile(file, 'utf8');
      assert.equal(retained, bytes, 'retained thinking level was rewritten');
      assert.match(retained, new RegExp(`"thinkingLevel":"${level}"`), 'exact thinking level was not preserved');
    } finally { await rm(root, { recursive: true, force: true }); }
  }
});

test('unknown thinking level fails closed without rewriting retained state', async () => {
  const root = await mkdtemp('/tmp/pi-state-test-');
  try {
    const { manager } = await openDurableSession(identity, root);
    const file = manager.getSessionFile();
    const header = await readFile(file, 'utf8');
    const entries = [
      { type: 'message', id: '11111111', parentId: null, message: { role: 'user', content: 'prior context', timestamp: 1 } },
      { type: 'thinking_level_change', id: '22222222', parentId: '11111111', thinkingLevel: 'ultra' },
    ];
    const bytes = header + entries.map(entry => JSON.stringify({ timestamp: '2026-10-09T12:00:00Z', ...entry }) + '\n').join('');
    await writeFile(file, bytes);
    await assert.rejects(openDurableSession({ ...identity, mustExist: true }, root), 'unknown thinking level was accepted');
    assert.equal(await readFile(file, 'utf8'), bytes, 'unknown thinking level rewrote retained state');
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('wrong header, duplicate entry identity, truncation, old format and symlinks fail closed', async () => {
  for (const corruption of ['wrong-header', 'duplicate', 'truncated', 'old-format', 'symlink']) {
    const root = await mkdtemp('/tmp/pi-state-test-');
    try {
      const { manager } = await openDurableSession(identity, root);
      const file = manager.getSessionFile();
      const original = await readFile(file, 'utf8');
      const header = JSON.parse(original);
      let bytes = original;
      if (corruption === 'wrong-header') bytes = JSON.stringify({ ...header, id: 'wrong-session' }) + '\n';
      if (corruption === 'old-format') bytes = JSON.stringify({ ...header, version: 2 }) + '\n';
      if (corruption === 'duplicate') {
        const entry = JSON.stringify({ type: 'custom', id: 'aaaabbbb', parentId: null, timestamp: header.timestamp, customType: 'fixture' }) + '\n';
        bytes += entry + entry;
      }
      if (corruption === 'truncated') bytes += '{"type":"message"';
      if (corruption === 'symlink') {
        const other = join(root, 'other.jsonl');
        await writeFile(other, original);
        await rm(file);
        await symlink(other, file);
      } else await writeFile(file, bytes);
      await assert.rejects(openDurableSession(identity, root), corruption);
      assert.equal(await readFile(file, 'utf8'), bytes, 'invalid input must not be migrated or rewritten');
    } finally { await rm(root, { recursive: true, force: true }); }
  }
});
