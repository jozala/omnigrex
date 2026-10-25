import assert from 'node:assert/strict';
import { test } from 'node:test';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const fixture = fileURLToPath(new URL('./control-rpc-fixture.mjs', import.meta.url));
const body = 'LONG_RPC_BODY'.repeat(40000);
async function prompt(t, root, mode, continued = false) {
  const child = spawn(process.execPath, [fixture, root, mode, continued ? 'continued' : ''],
    { env: { ...process.env, PI_OFFLINE: '1', PI_TELEMETRY: '0', PI_SKIP_VERSION_CHECK: '1' }, stdio: ['pipe', 'pipe', 'pipe'] });
  t.after(() => { if (child.exitCode === null) child.kill('SIGKILL'); });
  const records = [];
  let pending = Buffer.alloc(0), largest = 0, deltas = 0, stderr = '', resolveSettled, rejectSettled;
  const settled = new Promise((resolve, reject) => { resolveSettled = resolve; rejectSettled = reject; });
  const timer = setTimeout(() => rejectSettled(new Error('controlled RPC settlement timed out')), 30000);
  const send = command => child.stdin.write(JSON.stringify(command) + '\n');
  child.stderr.on('data', chunk => { stderr = (stderr + chunk).slice(-2048); });
  child.on('error', rejectSettled);
  const exited = new Promise(resolve => child.once('exit', (code, signal) => {
    if (!records.some(record => record.type === 'agent_settled')) rejectSettled(new Error(`fixture exited before settlement: ${code} ${signal}; ${stderr}`));
    resolve({ code, signal });
  }));
  child.stdout.on('data', chunk => {
    try {
      pending = Buffer.concat([pending, chunk]);
      for (;;) {
        const end = pending.indexOf(10);
        if (end < 0) break;
        assert.ok(end < 1 << 20, 'RPC frame exceeds existing controller limit');
        largest = Math.max(largest, end);
        const record = JSON.parse(pending.subarray(0, end));
        pending = pending.subarray(end + 1);
        records.push(record);
        if (record.type === 'message_start' && record.message.role === 'assistant') {
          // Exercise command correlation while streaming and genuine stdout
          // backpressure without changing the stock RPC writer.
          send({ type: 'get_state', id: 'during-stream' });
          child.stdout.pause();
          setTimeout(() => child.stdout.resume(), 50);
        }
        if (record.type === 'message_update' && record.assistantMessageEvent.type === 'text_delta') {
          deltas++;
          if (mode === 'cancel' && deltas === 7) send({ type: 'abort', id: 'cancel-long' });
        }
        if (record.type === 'agent_settled') resolveSettled();
      }
      assert.ok(pending.length < 1 << 20, 'unframed required record exceeds limit');
    } catch (error) { rejectSettled(error); }
  });
  send({ type: 'prompt', id: 'long-prompt', message: continued ? 'continue' : 'long synthetic turn' });
  try {
    await settled;
    child.stdin.end();
    const exit = await exited;
    assert.equal(exit.code, 0, `cleanup failed: ${exit.signal}; ${stderr}`);
  } finally { clearTimeout(timer); }
  assert.ok(largest < 4096, `control frame unexpectedly large: ${largest}`);
  assert.ok(records.some(record => record.type === 'response' && record.id === 'long-prompt' && record.success && record.data.disposition === 'started'));
  assert.ok(records.some(record => record.type === 'response' && record.id === 'during-stream' && record.command === 'get_state' && record.success));
  const end = records.findLastIndex(record => record.type === 'agent_end');
  const receipt = records.findLastIndex(record => record.type === 'agent_settled');
  assert.ok(end >= 0 && receipt > end, 'agent_settled must follow agent_end');
  assert.equal(records[receipt].aborted, mode === 'cancel');
  for (const record of records.filter(record => record.type === 'agent_end')) {
    assert.equal(typeof record.willRetry, 'boolean');
    assert.equal('messages' in record, false);
    assert.ok(Buffer.byteLength(JSON.stringify(record)) < 64);
  }
  const reason = records.findLast(record => record.type === 'message_end' && record.message.role === 'assistant')?.message.stopReason;
  assert.equal(reason, mode === 'cancel' ? 'aborted' : mode === 'error' ? 'error' : 'stop');
  if (mode === 'retry') {
    assert.ok(records.some(record => record.type === 'agent_end' && record.willRetry));
    assert.ok(records.some(record => record.type === 'auto_retry_start'));
    assert.ok(records.some(record => record.type === 'auto_retry_end' && record.success));
  }
  if (mode === 'cancel') assert.ok(records.some(record => record.type === 'response' && record.id === 'cancel-long' && record.command === 'abort' && record.success));
  return records;
}

for (const mode of ['normal', 'retry', 'error', 'cancel']) {
  test(`stock Pi RPC projects multi-megabyte ${mode} completion`, { timeout: 45000 }, async t => {
    const root = await mkdtemp(join(tmpdir(), 'pi-control-rpc-'));
    t.after(() => rm(root, { recursive: true, force: true }));
    await prompt(t, root, mode);
    if (mode === 'normal') {
      const path = await readFile(join(root, 'session-path'), 'utf8');
      const lines = (await readFile(path, 'utf8')).trim().split('\n');
      assert.ok(lines.every(line => Buffer.byteLength(line) < 1 << 20), 'fixture violated durable entry bound');
      const entries = lines.map(line => JSON.parse(line));
      assert.equal(entries.filter(entry => entry.type === 'message' && entry.message.role === 'assistant' && entry.message.content.some(block => block.type === 'text' && block.text === body)).length, 6, 'durable Session lost complete assistant messages');
      await prompt(t, root, 'normal', true);
    }
  });
}
