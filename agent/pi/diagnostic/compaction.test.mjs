// Native Pi, production settings/extensions and durable Session state. No paid
// provider, custom summary hook, threshold override, or controller recovery.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import http from 'node:http';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { prepareRuntime } from '../runner.mjs';
import { openDurableSession } from '../session-state.mjs';
import { controlRuntime } from '../control-events.mjs';
import { historyInput } from '../history-rpc.mjs';
import { PassThrough } from 'node:stream';

const oldBody = 'RETAINED_APPEND_ORDER_HISTORY '.repeat(1200);
const summary = 'CHECKPOINT: continue the original assignment. OLD_DEPLOYMENT_HINT is not authoritative.';
const secret = 'SYNTHETIC_PROVIDER_SECRET';
const usage = input => ({ input, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: input + 1,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } });
const text = content => typeof content === 'string' ? content : content?.filter(b => b.type === 'text').map(b => b.text).join('') ?? '';

async function fixture(t, mode) {
  const root = await mkdtemp(join(tmpdir(), 'pi-native-compaction-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const id = randomUUID();
  const identity = { agentSessionId: id, runtimeSessionId: id, mustExist: false };
  const { manager } = await openDurableSession(identity, root);
  for (let i = 0; i < 8; i++) {
    manager.appendMessage({ role: 'user', content: `prior request ${i} ${oldBody}`, timestamp: 1 + i * 2 });
    manager.appendMessage({ role: 'assistant', content: [{ type: 'text', text: `prior answer ${i}` }],
      api: 'openai-completions', provider: 'diagnostic', model: 'controlled', stopReason: 'stop',
      usage: usage(mode.startsWith('preprompt') && i === 7 ? 60000 : 1000), timestamp: 2 + i * 2 });
  }
  const before = await readFile(manager.getSessionFile(), 'utf8');
  const requests = [], wire = [];
  let summaries = 0, conversations = 0, runtime;
  const server = http.createServer(async (request, response) => {
    if (request.method !== 'POST') { response.writeHead(405); response.end(); return; }
    let raw = '';
    for await (const part of request) raw += part;
    const data = JSON.parse(raw);
    if (request.url === '/mcp') {
      if (!('id' in data)) { response.writeHead(202); response.end(); return; }
      const result = data.method === 'initialize'
        ? { protocolVersion: data.params.protocolVersion, capabilities: { tools: {} }, serverInfo: { name: 'compaction-fixture', version: '1' } }
        : data.method === 'tools/list' ? { tools: [] } : {};
      response.writeHead(200, { 'Content-Type': 'application/json' });
      response.end(JSON.stringify({ jsonrpc: '2.0', id: data.id, result })); return;
    }
    const maintenance = data.messages.some(m => m.role === 'system' && text(m.content).includes('You are a context summarization assistant'));
    requests.push({ maintenance, messages: data.messages });
    if (maintenance) {
      summaries++;
      if (mode === 'cancel' || mode === 'preprompt-cancel') { setTimeout(() => { void runtime.session.abort(); }, 10); return; }
      if (mode === 'summary-failure') {
        response.writeHead(503, { 'Content-Type': 'application/json' });
        response.end(JSON.stringify({ error: { message: `unavailable ${secret}` } })); return;
      }
    } else {
      conversations++;
      if ((mode === 'overflow' && conversations === 1) || mode === 'overflow-guard') {
        response.writeHead(400, { 'Content-Type': 'application/json' });
        response.end(JSON.stringify({ error: { message: `Your input exceeds the context window of this model ${secret}` } })); return;
      }
    }
    response.writeHead(200, { 'Content-Type': 'text/event-stream' });
    const chunk = (delta, finish_reason = null, extra = {}) => response.write(`data: ${JSON.stringify({ id: 'compaction-fixture', object: 'chat.completion.chunk', created: 1, model: 'controlled', choices: [{ index: 0, delta, finish_reason }], ...extra })}\n\n`);
    chunk({ role: 'assistant', content: maintenance ? summary : 'original assignment continues' });
    chunk({}, 'stop', { usage: { prompt_tokens: !maintenance && conversations === 1 && ['threshold', 'summary-failure', 'cancel'].includes(mode) ? 60000 : 1000, completion_tokens: 1, total_tokens: 1001 } });
    response.end('data: [DONE]\n\n');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => { server.closeAllConnections(); server.close(); });
  const url = `http://127.0.0.1:${server.address().port}`;
  const models = { providers: { diagnostic: { baseUrl: `${url}/v1`, api: 'openai-completions',
    models: [{ id: 'controlled', name: 'Controlled', reasoning: false, input: ['text'], contextWindow: 65536, maxTokens: 1024,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }] } } };
  const agentDir = join(root, 'agent');
  const config = instructions => ({ auth: { diagnostic: { type: 'api_key', key: `fixture-${mode}` } },
    instructions, selection: { model: 'diagnostic/controlled', thinking: 'off' }, repositorySkills: false,
    tools: [], mcpTools: [], mcp: { url: `${url}/mcp`, headers: [] } });
  async function open(manager, instructions) {
    runtime = await prepareRuntime(config(instructions), manager, models, { agentDir });
    controlRuntime(runtime).session.subscribe(e => wire.push(e));
    return runtime;
  }
  await open(manager, 'CURRENT_DEPLOYMENT_INSTRUCTIONS');
  t.after(async () => { if (runtime) await runtime.dispose(); });
  return { manager, identity, root, agentDir, before, requests, wire, open,
    get runtime() { return runtime; }, get summaries() { return summaries; }, get conversations() { return conversations; } };
}

test('compaction fixtures preserve another runtime’s credentials and model catalog', { timeout: 30000 }, async t => {
  const parent = await fixture(t, 'parent');
  const authPath = join(parent.agentDir, 'auth.json');
  const modelsPath = join(parent.agentDir, 'models.json');
  const authBefore = await readFile(authPath, 'utf8');
  const modelsBefore = await readFile(modelsPath, 'utf8');

  const child = await fixture(t, 'child');
  assert.notEqual(child.agentDir, parent.agentDir);
  assert.equal(await readFile(authPath, 'utf8'), authBefore);
  assert.equal(await readFile(modelsPath, 'utf8'), modelsBefore);
  assert.equal(JSON.parse(await readFile(join(child.agentDir, 'auth.json'), 'utf8')).diagnostic.key, 'fixture-child');
  const model = parent.runtime.services.modelRuntime.getModel('diagnostic', 'controlled');
  const auth = await parent.runtime.services.modelRuntime.getAuth(model);
  assert.equal(auth.auth.apiKey, 'fixture-parent', 'the live runtime must retain its own credential');

  await parent.runtime.session.prompt('parent continues after fixture initialization');
  assert.equal(parent.conversations, 1);
  assert.equal(child.conversations, 0, 'the parent must still use its own provider endpoint');
  await child.runtime.session.prompt('child uses its own fixture');
  assert.equal(child.conversations, 1);
});

test('cancellation during the compatibility wait preserves native settlement evidence', { timeout: 30000 }, async t => {
  const f = await fixture(t, 'threshold');
  const append = f.manager.appendCompaction.bind(f.manager);
  const realNow = Date.now;
  t.after(() => { Date.now = realNow; });
  let cancellation, cancelledBeforeCompactionEnd;
  f.manager.appendCompaction = (...args) => {
    const result = append(...args);
    // Freeze only after real persistence so the production hook must yield.
    const boundary = Date.parse(f.manager.getBranch().at(-1).timestamp);
    Date.now = () => boundary;
    setTimeout(() => {
      cancelledBeforeCompactionEnd = !f.wire.some(event => event.type === 'compaction_end');
      cancellation = f.runtime.session.abort();
      Date.now = realNow;
    }, 0);
    return result;
  };
  f.runtime.session.subscribe(event => {
    if (event.type === 'compaction_start') void f.runtime.session.followUp('continue original assignment');
  });
  await f.runtime.session.prompt('continue original assignment');
  Date.now = realNow;
  assert.ok(cancellation, 'compatibility wait must allow cancellation to run');
  await cancellation;
  assert.equal(cancelledBeforeCompactionEnd, true, 'cancellation must run inside the awaited compatibility hook, not during later continuation');
  assert.equal(f.summaries, 1);
  assert.equal(f.conversations, 1, 'cancelled continuation must not contact the provider');
  assert.equal(f.wire.find(event => event.type === 'compaction_end').aborted, false);
  const settled = f.wire.filter(event => event.type === 'agent_settled');
  assert.equal(settled.length, 1);
  assert.equal(settled[0].aborted, true, 'successful compaction must not erase cancellation evidence');
});

for (const mode of ['threshold', 'overflow', 'overflow-guard', 'summary-failure', 'cancel', 'preprompt', 'preprompt-cancel']) {
  test(`native compaction: ${mode}`, { timeout: 30000 }, async t => {
    const f = await fixture(t, mode);
    if (mode === 'threshold') f.runtime.session.subscribe(e => {
      if (e.type === 'compaction_start') void f.runtime.session.followUp('continue original assignment');
    });
    await f.runtime.session.prompt('continue original assignment');
    assert.ok(f.summaries >= 1, 'native summary request must run');
    assert.equal(f.wire.filter(e => e.type === 'agent_settled').length, 1, 'compaction must not settle separately');
    assert.equal(f.wire.some(e => e.type === 'auto_retry_start'), false, 'ordinary provider retries remain disabled');
    assert.ok(f.wire.some(e => e.type === 'compaction_start' && e.reason === (mode.startsWith('overflow') ? 'overflow' : 'threshold')));
    const durable = await readFile(f.manager.getSessionFile(), 'utf8');
    assert.ok(durable.startsWith(f.before), 'append-order history must remain intact');
    assert.equal(durable.includes(secret), false, 'provider failure text must not persist');
    assert.equal(JSON.stringify(f.wire).includes(secret), false, 'provider failure text must not reach control diagnostics');
    assert.equal(JSON.stringify(f.wire).includes(summary), false, 'summary body must not reach control diagnostics');
    const ends = f.wire.filter(e => e.type === 'compaction_end');
    if (mode === 'summary-failure') {
      assert.equal(f.summaries, 1, 'summary failure must not start ordinary retries');
      assert.equal(ends.at(-1).errorMessage, 'Omnigrex compaction_failed');
      assert.equal(durable.includes('"type":"compaction"'), false);
      return;
    }
    if (mode === 'cancel' || mode === 'preprompt-cancel') {
      assert.equal(ends.at(-1).aborted, true);
      // Pi retains the prior stop after post-run cancellation and may emit a
      // preflight error after pre-prompt cancellation. The adapter combines
      // aborted compaction and its cancellation request at agent_settled.
      assert.equal(ends.at(-1).willRetry, false);
      assert.equal(f.conversations, mode === 'preprompt-cancel' ? 0 : 1, 'cancel must not start another conversational request');
      assert.equal(durable.includes('"type":"compaction"'), false);
      return;
    }
    assert.ok(durable.includes(summary), 'summary belongs in protected Session state');
    if (mode === 'overflow-guard') {
      assert.equal(f.summaries, 1, 'exhausted recovery must not compact again');
      assert.equal(f.conversations, 2, 'native overflow recovery allows only one retry');
      assert.equal(ends.at(-1).errorMessage, 'Omnigrex compaction_failed');
      assert.equal(ends.at(-1).willRetry, false);
      return;
    }
    const conversation = f.requests.filter(r => !r.maintenance);
    if (mode === 'preprompt') {
      assert.equal(f.requests[0].maintenance, true);
      assert.equal(JSON.stringify(f.requests[0]).includes('continue original assignment'), false, 'new prompt must follow pre-prompt summary');
    } else {
      assert.equal(f.conversations, 2, 'same assignment continues natively');
      assert.ok(JSON.stringify(conversation[1]).length < JSON.stringify(conversation[0]).length / 2, 'model-visible context must shrink');
    }
    await f.runtime.dispose();
    const { manager } = await openDurableSession({ ...f.identity, mustExist: true }, f.root);
    assert.equal(manager.getSessionId(), f.identity.agentSessionId);
    assert.equal(manager.getEntries().filter(e => e.type === 'message' && e.message.role === 'user' && e.message.content.includes?.('prior request')).length, 8);
    await f.open(manager, 'REFRESHED_DEPLOYMENT_INSTRUCTIONS');
    // History Replay still exposes append-order metadata, not compacted context.
    const source = new PassThrough();
    const pages = [];
    const input = historyInput(f.runtime, source, (line, callback) => { pages.push(JSON.parse(line)); callback(); });
    input.resume();
    source.end(JSON.stringify({ type: 'omnigrex_get_history', id: 'history' }) + '\n');
    await new Promise((resolve, reject) => { input.on('end', resolve); input.on('error', reject); });
    assert.equal(pages[0].data.sessionId, f.identity.agentSessionId);
    assert.ok(pages[0].data.entries.some(e => e.type === 'compaction'));
    assert.equal(pages[0].data.entries.filter(e => e.type === 'message' && e.message.role === 'user').length >= 8, true);
    await f.runtime.session.prompt('reopened continuation');
    const latest = f.requests.at(-1).messages;
    const systems = latest.filter(m => m.role === 'system').map(m => text(m.content)).join('\n');
    assert.ok(systems.includes('REFRESHED_DEPLOYMENT_INSTRUCTIONS'));
    assert.equal(systems.includes('CURRENT_DEPLOYMENT_INSTRUCTIONS'), false);
    assert.equal(systems.includes('OLD_DEPLOYMENT_HINT'), false);
    assert.ok(JSON.stringify(latest).includes(summary));
  });
}
