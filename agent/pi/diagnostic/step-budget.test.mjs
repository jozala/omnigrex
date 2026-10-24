import assert from 'node:assert/strict';
import { test } from 'node:test';
import http from 'node:http';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createAgentSessionServices, createAgentSessionFromServices, SettingsManager, SessionManager } from '@earendil-works/pi-coding-agent';
import { stepBudget, summaryInstruction } from './step-budget.mjs';

async function fixture(t, limit, response, outer = false) {
  const root = await mkdtemp(join(tmpdir(), 'omnigrex-budget-'));
  const requests = [], calls = [], observations = [];
  let session, pending = 0;
  const budget = stepBudget(limit, (data, summary) => observations.push({ ...data, text: summary }));
  const server = http.createServer(async (req, res) => {
    let raw = ''; for await (const c of req) raw += c;
    const data = JSON.parse(raw); requests.push(data);
    assert.equal(pending, 0, 'preceding tool batch must finish before next request');
    const result = response(requests.length, data);
    if (result.hold) return;
    if (result.failure) { res.writeHead(400, { 'Content-Type': 'application/json' }); res.end(JSON.stringify({ error: { message: 'Controlled provider rejection' } })); return; }
    res.writeHead(200, { 'Content-Type': 'text/event-stream' });
    const delta = result.tools ? { role: 'assistant', tool_calls: result.tools.map((name, index) => ({ index, id: `call-${requests.length}-${index}`, type: 'function', function: { name, arguments: '{}' } })) } : { role: 'assistant', content: result.text ?? 'Work is incomplete; verification remains.' };
    for (const [d, f] of [[delta, null], [{}, result.reason ?? (result.tools ? 'tool_calls' : 'stop')]]) res.write(`data: ${JSON.stringify({ id: 'fixture', object: 'chat.completion.chunk', created: 1, model: 'controlled', choices: [{ index: 0, delta: d, finish_reason: f }] })}\n\n`);
    res.end('data: [DONE]\n\n');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => { session?.dispose(); server.closeAllConnections(); await new Promise(r => server.close(r)); await rm(root, { recursive: true, force: true }); });
  await writeFile(join(root, 'auth.json'), JSON.stringify({ controlled: { type: 'api_key', key: 'fixture-only' } }));
  await writeFile(join(root, 'models.json'), JSON.stringify({ providers: { controlled: { baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: 'openai-completions', models: [{ id: 'controlled', name: 'controlled', reasoning: false, input: ['text'], contextWindow: 32000, maxTokens: 1024, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }] } } }));
  const services = await createAgentSessionServices({ cwd: root, agentDir: root,
    settingsManager: SettingsManager.inMemory({ retry: { enabled: false }, compaction: { enabled: false }, cacheWarming: 'off' }, { projectTrusted: false }),
    resourceLoaderOptions: { noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true, noContextFiles: true, disabledBuiltinExtensions: ['mcp'], systemPromptOverride: () => 'FORCED_SYSTEM', extensionFactories: [budget.extension, pi => {
      for (const name of ['probe', 'omnigrex_fixture', 'deferred_fixture']) pi.registerTool({ name, label: name, description: name, ...(name === 'deferred_fixture' ? { exposure: 'deferred' } : {}), parameters: { type: 'object', properties: {} }, async execute(_id, _args, _signal, _update, ctx) {
        if (name === 'probe') await ctx.executeTool('deferred_fixture', {});
        pending++; await new Promise(r => setTimeout(r, 5)); calls.push(name); pending--;
        return { content: [{ type: 'text', text: 'ok' }], details: {} };
      } });
      if (outer) {
        if (outer === true || outer === 'turn') pi.on('turn_end', () => ({ entries: [{ type: 'custom_message', customType: 'budget-fixture', content: 'Continue fixture work', display: false }], continue: true }));
        if (outer === true || outer === 'queued') pi.on('agent_end', () => { pi.sendUserMessage('queued continuation', { deliverAs: 'followUp' }); });
        if (outer === true || outer === 'settle') pi.on('agent_before_settle', () => ({ entries: [{ type: 'custom_message', customType: 'budget-fixture', content: 'Continue fixture work', display: false }], continue: true }));
      }
    }] } });
  ({ session } = await createAgentSessionFromServices({ services, sessionManager: SessionManager.inMemory(root), model: services.modelRuntime.getModel('controlled', 'controlled'), tools: ['probe', 'omnigrex_fixture', 'deferred_fixture'] }));
  budget.attach(session, ['probe', 'omnigrex_fixture']);
  await session.bindExtensions({});
  return { session, requests, calls, observations };
}

for (const limit of [1, 3]) test(`limit ${limit}: no N+1, no executable or declared summary tools, outer continuations fenced`, async t => {
  const f = await fixture(t, limit, () => ({ tools: ['probe', 'omnigrex_fixture', 'deferred_fixture'] }), true);
  await f.session.prompt('work');
  assert.equal(f.requests.length, limit);
  assert.equal(f.calls.length, (limit - 1) * 3);
  assert.equal(f.requests.at(-1).tools?.length ?? 0, 0);
  assert.ok(JSON.stringify(f.requests.at(-1).messages).includes(summaryInstruction));
  for (const request of f.requests.slice(0, -1)) assert.ok(!JSON.stringify(request).includes(summaryInstruction));
  assert.deepEqual(f.observations.at(-1), { limit, used: limit, responses: limit, summary: true, stopped: true, complete: true, text: '' });
  assert.equal(f.session.agent.hasQueuedMessages(), false);
  const start = f.requests.length;
  await f.session.prompt('new admitted Turn');
  assert.equal(f.requests.length - start, limit);
  assert.equal(f.requests[start].tools?.length ?? 0, limit === 1 ? 0 : 2);
});

test('early completion and unlimited usage do not force summary; transcript history does not count', async t => {
  const f = await fixture(t, 'unlimited', n => n === 1 ? { tools: ['probe', 'omnigrex_fixture'] } : {});
  await f.session.prompt('first Turn');
  assert.equal(f.requests.length, 2);
  assert.equal(f.observations.at(-1).used, 2);
  assert.equal(f.observations.at(-1).summary, false);
  await f.session.prompt('continued Session');
  assert.equal(f.observations.at(-1).used, 1);
  assert.ok(!JSON.stringify(f.requests).includes(summaryInstruction));
});

test('finite early completion does not invoke final iteration', async t => {
  const f = await fixture(t, 5, () => ({}));
  await f.session.prompt('work');
  assert.equal(f.requests.length, 1);
  assert.equal(f.observations.at(-1).stopped, false);
  assert.equal(f.observations.at(-1).used, 1);
});

test('token-length limit at summary boundary is not positive step exhaustion', async t => {
  const f = await fixture(t, 1, () => ({ reason: 'length' }), true);
  await f.session.prompt('work');
  assert.equal(f.requests.length, 1);
  assert.equal(f.observations.at(-1).stopped, false);
  assert.equal(f.observations.at(-1).complete, true);
});

for (const outer of ['turn', 'queued', 'settle']) test(`natural completion followed by ${outer} continuation shares the same allowance`, async t => {
  const f = await fixture(t, 3, () => ({}), outer);
  await f.session.prompt('work');
  assert.equal(f.requests.length, 3);
  assert.equal(f.observations.at(-1).used, 3);
  assert.equal(f.observations.at(-1).complete, true);
  assert.equal(f.observations.at(-1).stopped, true);
  assert.equal(f.session.agent.hasQueuedMessages(), false);
});

test('summary text is observed, instructions do not leak into the next Turn', async t => {
  const f = await fixture(t, 2, n => n === 1 ? { tools: ['probe'] } : {});
  await f.session.prompt('work');
  assert.equal(f.observations.at(-1).text, 'Work is incomplete; verification remains.');
  await f.session.prompt('continue the Session');
  assert.equal(f.observations.at(-1).used, 1);
  assert.equal(f.observations.at(-1).summary, false);
  assert.ok(!JSON.stringify(f.requests.at(-1).messages).includes(summaryInstruction));
  assert.equal(f.requests.at(-1).tools.length, 2);
});

test('provider failure in summary-only mode is not step exhaustion or a retry opportunity', async t => {
  const f = await fixture(t, 1, () => ({ failure: true }), true);
  await f.session.prompt('work');
  assert.equal(f.requests.length, 1);
  assert.equal(f.observations.at(-1).stopped, false);
  assert.equal(f.observations.at(-1).responses, 1);
  assert.equal(f.observations.at(-1).complete, true);
});

test('cancellation in summary-only mode is not step exhaustion', async t => {
  const f = await fixture(t, 1, () => ({ hold: true }), true);
  const prompt = f.session.prompt('work');
  while (!f.requests.length) await new Promise(r => setTimeout(r, 5));
  await f.session.abort(); await prompt;
  assert.equal(f.requests.length, 1);
  assert.equal(f.observations.at(-1).stopped, false);
  assert.equal(f.observations.at(-1).complete, true);
});


test('oversized summaries are omitted wholesale rather than exposing pre-redaction credential fragments', async t => {
  const f = await fixture(t, 1, () => ({ text: 'unfinished '.repeat(400) + 'fixture-only-secret' }));
  await f.session.prompt('work');
  assert.equal(f.observations.at(-1).text, '');
  assert.equal(f.observations.at(-1).used, 1);
  assert.equal(f.observations.at(-1).stopped, true);
  assert.equal(f.observations.at(-1).complete, true);
});
