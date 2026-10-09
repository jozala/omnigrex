import assert from 'node:assert/strict';
import { test } from 'node:test';
import { isRetryableAssistantError } from '@earendil-works/pi-ai/compat';
import { failureExtension } from './failure-extension.mjs';

const secret = 'failure-boundary-fixture-secret';
const model = { api: 'openai-completions', provider: 'diagnostic', id: 'controlled' };

for (const [stopReason, errorMessage, retryable] of [
  ['error', `400 Invalid API key: ${secret}`, false],
  ['error', `503 Service unavailable: ${secret}`, true],
  ['error', `429 insufficient_quota: ${secret}`, false],
  ['aborted', `Request aborted: ${secret}`, false],
]) {
  test(`pre-persistence ${stopReason} preserves failure and retry classification`, () => {
    let finalize;
    failureExtension({ on(name, handler) { assert.equal(name, 'message_end'); finalize = handler; } });
    const source = {
      role: 'assistant', stopReason, errorMessage, api: secret, provider: secret, model: secret,
      content: [{ type: 'text', text: secret }, { type: 'thinking', thinking: secret },
        { type: 'toolCall', id: secret, name: 'read', arguments: { path: secret } }],
      responseId: secret, responseModel: secret, rawStopReason: secret, diagnostics: [{ error: { message: secret } }],
      deferred: { id: secret }, providerThinkingLevel: secret, timestamp: 123,
      usage: { input: 9, output: 2, cacheRead: 1, cacheWrite: secret, totalTokens: 12,
        cost: { input: 0.25, output: secret, cacheRead: 0, cacheWrite: 0, total: 0.25 } },
    };
    const { message } = finalize({ message: source }, { model });
    assert.equal(message.role, 'assistant');
    assert.equal(message.stopReason, stopReason);
    assert.equal(isRetryableAssistantError(message), retryable);
    assert.equal(JSON.stringify(message).includes(secret), false);
    assert.equal(message.provider, model.provider);
    assert.equal(message.usage.input, 9);
    assert.equal(message.usage.cost.total, 0.25);
    assert.equal(message.timestamp, 123);
    // Returning a replacement lets Pi update all references before listeners
    // and SessionManager see the message; this hook does not modify history.
    assert.equal(source.errorMessage, errorMessage);
  });
}

test('successful messages and non-provider messages remain intact', () => {
  let finalize;
  failureExtension({ on(_name, handler) { finalize = handler; } });
  for (const source of [{ role: 'assistant', stopReason: 'stop', content: [{ type: 'text', text: 'useful response' }] },
    { role: 'toolResult', isError: true, content: [{ type: 'text', text: 'safe tool error' }] }]) {
    const original = JSON.stringify(source);
    assert.equal(finalize({ message: source }, { model }), undefined);
    assert.equal(JSON.stringify(source), original);
  }
});

test('an invalidated context cannot bypass pre-persistence sanitation', () => {
  let finalize;
  failureExtension({ on(_name, handler) { finalize = handler; } });
  const context = { get model() { throw new Error(`stale context: ${secret}`); } };
  const { message } = finalize({ message: { role: 'assistant', stopReason: 'error', errorMessage: secret } }, context);
  assert.equal(message.stopReason, 'error');
  assert.equal(JSON.stringify(message).includes(secret), false);
});
