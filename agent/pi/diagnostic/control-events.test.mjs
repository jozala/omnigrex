import assert from 'node:assert/strict';
import { test } from 'node:test';
import { controlEvent, controlRuntime } from '../control-events.mjs';

const transcript = 'synthetic transcript '.repeat(160000);
const message = { role: 'assistant', content: [{ type: 'text', text: transcript }], stopReason: 'stop' };

test('multi-megabyte agent_end is projected without even reading messages', () => {
  const source = { type: 'agent_end', willRetry: false, messages: [message] };
  assert.ok(Buffer.byteLength(JSON.stringify(source)) > 3 << 20);
  Object.defineProperty(source, 'messages', { get() { throw new Error('transcript accessed'); } });
  const wire = JSON.stringify(controlEvent(source));
  assert.equal(wire, '{"type":"agent_end","willRetry":false}');
  assert.ok(Buffer.byteLength(wire) < 64);
  assert.deepEqual(controlEvent({ type: 'agent_end', willRetry: true, messages: [message] }), { type: 'agent_end', willRetry: true });
});

test('only the RPC subscriber sees the projection; unsubscribe and rebind retain authority', () => {
  class Session {
    #listeners = new Set();
    subscribe(listener) { this.#listeners.add(listener); return () => this.#listeners.delete(listener); }
    emit(event) { for (const listener of this.#listeners) listener(event); }
    get count() { return this.#listeners.size; }
  }
  class Runtime {
    #session = new Session();
    get session() { return this.#session; }
    replace() { this.#session = new Session(); }
  }
  const runtime = new Runtime(), view = controlRuntime(runtime);
  const durable = [], wire = [];
  runtime.session.subscribe(event => durable.push(event));
  const unsubscribe = view.session.subscribe(event => wire.push(event));
  const source = { type: 'agent_end', willRetry: false, messages: [message] };
  runtime.session.emit(source);
  runtime.session.emit({ type: 'agent_settled' });
  assert.equal(durable[0], source);
  assert.equal(durable[0].messages[0].content[0].text, transcript);
  assert.deepEqual(wire, [{ type: 'agent_end', willRetry: false }, { type: 'agent_settled' }]);
  unsubscribe();
  assert.equal(runtime.session.count, 1);
  const old = view.session;
  view.replace();
  assert.notEqual(view.session, old);
  view.session.subscribe(event => wire.push(event));
  runtime.session.emit({ type: 'agent_settled' });
  assert.equal(wire.length, 3);
});

test('transcript-heavy activity has explicit compact projections without mutating source', () => {
  const sources = [
    { type: 'message_start', message }, { type: 'message_end', message },
    { type: 'turn_end', message, toolResults: [message] },
    { type: 'entry_appended', entry: { type: 'message', message } },
    { type: 'tool_execution_start', toolCallId: 'call', toolName: 'bash', args: { command: transcript } },
    { type: 'tool_execution_update', toolCallId: 'call', toolName: 'bash', partialResult: message },
    { type: 'tool_execution_end', toolCallId: 'call', toolName: 'bash', result: message, isError: true },
    { type: 'queue_update', steering: [transcript], followUp: [] },
    { type: 'bash_execution_update', delta: transcript },
    { type: 'compaction_end', reason: 'manual', result: { summary: transcript }, aborted: false, willRetry: false },
    { type: 'message_update', message, assistantMessageEvent: { type: 'text_delta', delta: transcript, partial: message } },
  ];
  for (const source of sources) {
    const before = JSON.stringify(source);
    const projected = controlEvent(source);
    assert.equal(projected.type, source.type);
    assert.ok(Buffer.byteLength(JSON.stringify(projected)) < 256, source.type);
    assert.equal(JSON.stringify(source), before);
  }
});

test('outcome evidence, retry and unknown required fields are not truncated', () => {
  const end = { type: 'message_end', message: { ...message, stopReason: 'error', errorMessage: 'Omnigrex subscription_auth', omnigrexProviderGeneration: 'generation' } };
  assert.deepEqual(controlEvent(end).message, { role: 'assistant', stopReason: 'error', errorMessage: 'Omnigrex subscription_auth', omnigrexProviderGeneration: 'generation', omnigrexProviderDiagnostic: undefined });
  for (const source of [
    { type: 'agent_settled' },
    { type: 'auto_retry_start', attempt: 1, maxAttempts: 2, delayMs: 10, errorMessage: 'temporary' },
    { type: 'auto_retry_end', success: false, attempt: 1, finalError: 'Retry cancelled' },
    { type: 'future_required_event', required: transcript },
  ]) assert.equal(controlEvent(source), source);
  assert.throws(() => controlEvent({ type: 'message_end', message: null }));
});
