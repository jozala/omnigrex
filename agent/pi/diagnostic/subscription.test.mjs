import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { subscriptionRuntime, subscriptionPayload } from '../subscription-auth.mjs';
import { failureExtension } from '../failure-extension.mjs';

const configuration = { mcp: { url: 'http://controller.test:8081/mcp', headers: [{ name: 'Authorization', value: 'Bearer turn-authority' }] } };
const generations = ['00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002'];

test('subscription resolves credentials for successive model requests and preserves tool history', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'omnigrex-subscription-'));
  let tokens = 0;
  let requests = 0;
  const bodies = [];
  try {
    const subscription = await subscriptionRuntime(configuration, directory, null, async (url, init) => {
      if (String(url) === 'http://controller.test:8081/runtime/provider-auth') {
        assert.equal(init.headers.Authorization, 'Bearer turn-authority');
        assert.equal(init.redirect, 'error');
        const index = tokens++;
        return Response.json({ access_token: `access-${index}`, generation: generations[index], expires_at: new Date(Date.now() + 3600000).toISOString() });
      }
      assert.equal(String(url), 'https://api.openai.com/v1/responses');
      assert.equal(new Headers(init.headers).get('authorization'), `Bearer access-${requests}`);
      const body = JSON.parse(init.body);
      bodies.push(body);
      assert.equal(body.stream, true);
      assert.equal(body.store, false);
      assert.equal(body.tools, undefined);
      assert.ok(body.input.some(item => item.type === 'additional_tools' && item.tools[0].name === 'fixture_tool'));
      const item = requests++ === 0
        ? { type: 'function_call', id: 'fc_fixture', call_id: 'call_fixture', name: 'fixture_tool', arguments: '{}' }
        : { type: 'message', id: 'msg_fixture', role: 'assistant', content: [{ type: 'output_text', text: 'complete', annotations: [] }] };
      const events = [
        { type: 'response.created', response: { id: `resp_${requests}`, model: body.model } },
        { type: 'response.output_item.done', output_index: 0, item },
        { type: 'response.completed', response: { id: `resp_${requests}`, status: 'completed', output: [item], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } },
      ];
      return new Response(events.map(event => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join(''), { headers: { 'Content-Type': 'text/event-stream' } });
    });
    const model = subscription.runtime.getModel('openai', 'gpt-6.1-sol');
    assert.ok(model);
    const context = { systemPrompt: 'Use the fixture tool.', messages: [{ role: 'user', content: 'Run the tool.', timestamp: 1 }], tools: [{ name: 'fixture_tool', description: 'Fixture', parameters: { type: 'object', properties: {}, required: [] } }] };
    const first = await subscription.runtime.completeSimple(model, context);
    assert.equal(first.stopReason, 'toolUse');
    const call = first.content.find(block => block.type === 'toolCall');
    assert.equal(call.name, 'fixture_tool');
    context.messages.push(first, { role: 'toolResult', toolCallId: call.id, toolName: call.name, content: [{ type: 'text', text: 'tool-result' }], isError: false, timestamp: 2 });
    const second = await subscription.runtime.completeSimple(model, context);
    assert.equal(second.stopReason, 'stop');
    assert.equal(tokens, 2);
    assert.equal(requests, 2);
    assert.ok(bodies[1].input.some(item => item.type === 'function_call_output' && item.output.includes('tool-result')));
    assert.ok(!JSON.stringify(bodies).includes('access-'));
  } finally { await rm(directory, { recursive: true, force: true }); }
});

test('subscription payload follows direct-route requirements without losing context', () => {
  const payload = subscriptionPayload({ model: 'fixture', input: [{ type: 'message', role: 'system', content: 'guidance' }, { role: 'user', content: 'request' }], tools: [{ type: 'function', name: 'read', parameters: {} }], temperature: 0.5, max_output_tokens: 100, previous_response_id: 'old', metadata: { secret: 'not-supported' } });
  assert.equal(payload.input[0].role, 'developer');
  assert.equal(payload.input[0].content, 'guidance');
  assert.equal(payload.input[1].content, 'request');
  assert.equal(payload.input[2].type, 'additional_tools');
  for (const field of ['tools', 'temperature', 'max_output_tokens', 'previous_response_id', 'metadata']) assert.equal(payload[field], undefined);
  assert.throws(() => subscriptionPayload({ input: [], tools: [{ type: 'tool_search' }] }), /subscription_configuration/u);
});

test('quota classification is reported before sanitized failure persistence', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'omnigrex-subscription-'));
  const reports = [];
  try {
    const subscription = await subscriptionRuntime(configuration, directory, null, async (url, init) => {
      if (String(url) === 'http://controller.test:8081/runtime/provider-auth') {
        if (init.method === 'POST') { reports.push(JSON.parse(init.body)); return Response.json({}); }
        return Response.json({ access_token: 'access-secret', generation: generations[0], expires_at: new Date(Date.now() + 3600000).toISOString() });
      }
      return Response.json({ error: { code: 'subscription_sharing_usage_limit_exceeded', message: 'access-secret refresh-secret' } }, { status: 429 });
    });
    const model = subscription.runtime.getModel('openai', 'gpt-6.1-sol');
    const source = await subscription.runtime.completeSimple(model, { messages: [{ role: 'user', content: 'fixture', timestamp: 1 }] });
    assert.equal(source.stopReason, 'error');
    let finalize;
    failureExtension({ on(_event, handler) { finalize = handler; } }, subscription);
    const result = await finalize({ message: source }, { model });
    assert.deepEqual(reports, [{ generation: generations[0], failure: 'subscription_quota' }]);
    assert.equal(result.message.errorMessage, 'Omnigrex subscription_quota');
    assert.equal(result.message.omnigrexProviderGeneration, generations[0]);
    assert.ok(!JSON.stringify(result).includes('secret'));
  } finally { await rm(directory, { recursive: true, force: true }); }
});

test('only structured HTTP and SSE codes determine account-wide blocks', async t => {
  for (const scenario of [
    { status: 503, code: 'subscription_sharing_usage_unavailable', message: 'not subscription_sharing_usage_limit_exceeded', want: 'subscription_temporary' },
    { status: 404, code: 'model_not_found', message: 'This account cannot use the selected model.', want: 'subscription_configuration' },
    { code: 'subscription_sharing_usage_unavailable', message: 'not subscription_sharing_usage_limit_exceeded', want: 'subscription_temporary' },
    { code: 'subscription_sharing_usage_limit_exceeded', message: 'not subscription_sharing_usage_unavailable', want: 'subscription_quota' },
    { code: 'model_not_found', message: 'subscription_sharing_usage_limit_exceeded is only diagnostic prose', want: 'subscription_configuration' },
    { code: 'unknown_code', message: 'subscription_sharing_usage_limit_exceeded', want: 'subscription_temporary' },
    { status: 429, code: 'subscription_sharing_usage_limit_exceeded', message: 'limit', large: true, want: 'subscription_quota' },
    { code: 'subscription_sharing_usage_limit_exceeded', message: 'limit', large: true, want: 'subscription_quota' },
    { code: 'subscription_sharing_invalid_user', message: 'reauthorize', large: true, want: 'subscription_auth' },
    { code: 'subscription_sharing_usage_limit_exceeded', message: 'limit', ending: '\r', want: 'subscription_quota' },
    { code: 'subscription_sharing_invalid_user', message: 'reauthorize', ending: '\r', want: 'subscription_auth' },
    { code: 'subscription_sharing_usage_limit_exceeded', message: 'limit', ending: '\r', topLevel: true, large: true, want: 'subscription_quota' },
  ]) {
    await t.test(`${scenario.status ?? 'SSE'} ${scenario.code}${scenario.large ? ' large' : ''}${scenario.ending === '\r' ? ' CR' : ''}${scenario.topLevel ? ' top-level' : ''}`, async () => {
      const directory = await mkdtemp(join(tmpdir(), 'omnigrex-subscription-'));
      const reports = [];
      try {
        const subscription = await subscriptionRuntime(configuration, directory, null, async (url, init) => {
          if (String(url) === 'http://controller.test:8081/runtime/provider-auth') {
            if (init.method === 'POST') { reports.push(JSON.parse(init.body)); return Response.json({}); }
            return Response.json({ access_token: 'access-fixture', generation: generations[0], expires_at: new Date(Date.now() + 3600000).toISOString() });
          }
          const error = { code: scenario.code, message: scenario.message, ...(scenario.large ? { diagnostic: 'x'.repeat(70000) } : {}) };
          if (scenario.status) return Response.json({ error }, { status: scenario.status });
          const event = scenario.topLevel ? { type: 'error', ...error } : { type: 'response.failed', response: { id: 'resp_failed', status: 'failed', ...(scenario.large ? { output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'y'.repeat(70000) }] }] } : {}), error } };
          const ending = scenario.ending ?? '\r\n';
          const bytes = new TextEncoder().encode(`event: ${event.type}${ending}data: ${JSON.stringify(event)}${ending}${ending}`);
          // Deliberately split fields, codes, JSON, and CRLF across byte chunks.
          return new Response(new ReadableStream({ start(controller) { for (let i = 0; i < bytes.length; i += 7) controller.enqueue(bytes.slice(i, i + 7)); controller.close(); } }), { headers: { 'Content-Type': 'text/event-stream' } });
        });
        const model = subscription.runtime.getModel('openai', 'gpt-6.1-sol');
        const source = await subscription.runtime.completeSimple(model, { messages: [{ role: 'user', content: 'fixture', timestamp: 1 }] });
        assert.equal(source.stopReason, 'error');
        const classified = await subscription.failure(source);
        assert.equal(classified.failure, scenario.want);
        assert.equal(reports.length, ['subscription_quota', 'subscription_auth'].includes(scenario.want) ? 1 : 0);
      } finally { await rm(directory, { recursive: true, force: true }); }
    });
  }
});

test('unavailable controller does not fall back to ambient OpenAI credentials', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'omnigrex-subscription-'));
  const old = process.env.OPENAI_API_KEY;
  process.env.OPENAI_API_KEY = 'sk-must-not-be-used';
  try {
    const subscription = await subscriptionRuntime(configuration, directory, null, async () => Response.json({ failure: 'subscription_auth' }, { status: 503 }));
    await assert.rejects(subscription.runtime.getAuth('openai'), /subscription_auth/u);
    assert.equal((await subscription.failure({ errorMessage: '' })).failure, 'subscription_auth');
  } finally {
    if (old === undefined) delete process.env.OPENAI_API_KEY; else process.env.OPENAI_API_KEY = old;
    await rm(directory, { recursive: true, force: true });
  }
});
