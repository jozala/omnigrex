import test from 'node:test';
import assert from 'node:assert/strict';
import { safeDiagnostic, normalizedCause, requestIDHash } from '../provider-diagnostic.mjs';
import { failureExtension } from '../failure-extension.mjs';

const secret = 'SENTINEL-secret-prompt-https://private/?token=secret';
test('closed diagnostic schema drops malformed, oversized and throwing metadata', async () => {
  for (const source of [null, secret.repeat(10000), { stage: secret, cause: secret, auth_mode: secret, http_status: 999, code: secret, request_id_hash: secret, elapsed_ms: -1, attempt: secret, retryability: secret, message: secret }, new Proxy({}, { get() { throw Error(secret); } })]) {
    assert.ok(!JSON.stringify(safeDiagnostic(source)).includes('SENTINEL'));
  }
  assert.equal(normalizedCause(new Proxy({}, { get() { throw Error(secret); } })), 'unknown');
  assert.equal(requestIDHash(new Response('', { headers: { 'x-request-id': secret } })), null);
  assert.notEqual(requestIDHash(new Response('', { headers: { 'x-request-id': 'sk-secret-but-syntactically-valid' } })), 'sk-secret-but-syntactically-valid');
  for (const failure of [() => { throw Error(secret); }, async () => { throw Error(secret); }, async () => ({ failure: secret, generation: '', diagnostic: new Proxy({}, { get() { throw Error(secret); } }) })]) {
    let finalize;
    failureExtension({ on(_event, handler) { finalize = handler; } }, { failure });
    const result = await finalize({ message: { role: 'assistant', stopReason: 'error', errorMessage: secret, content: [{ type: 'text', text: secret }] } }, {});
    assert.equal(result.message.errorMessage, 'Omnigrex subscription_temporary');
    assert.ok(!JSON.stringify(result).includes(secret));
  }
});

test('stateful getters cannot pass an allowlist and then substitute a secret', () => {
  for (const [field, safe] of [['stage', 'local'], ['cause', 'http'], ['code', 'model_not_found'], ['retryability', 'unknown'], ['auth_mode', 'subscription'], ['attempt', '00000000-0000-4000-8000-000000000001'], ['request_id_hash', 'a'.repeat(64)]]) {
    let reads = 0;
    const source = { get [field]() { return reads++ === 0 ? safe : secret; } };
    assert.ok(!JSON.stringify(safeDiagnostic(source)).includes('SENTINEL'), field);
  }
});
