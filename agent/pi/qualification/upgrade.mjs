// Controlled upgrade provider only. Session persistence, replay and RPC use the
// production runner; every environment gets a fresh, request-counted provider.
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { readBootstrap, run } from '/opt/omnigrex/pi/runner.mjs';
import { openDurableSession } from '/opt/omnigrex/pi/session-state.mjs';

const text = value => typeof value === 'string' ? value : Array.isArray(value) ? value.filter(b => b.type === 'text').map(b => b.text).join('') : '';
try {
  const pkg = JSON.parse(await readFile('/opt/omnigrex/pi/node_modules/@earendil-works/pi-coding-agent/package.json', 'utf8'));
  if (pkg.version !== process.env.OMNIGREX_PI_EXPECTED_VERSION || process.version !== 'v24.14.1') throw new Error('unexpected runtime identity');
  const configuration = await readBootstrap();
  let count = 0;
  const server = http.createServer(async (request, response) => {
    if (request.url === '/mcp') {
      if (request.method !== 'POST') { response.writeHead(405); response.end(); return; }
      let body = '';
      for await (const part of request) body += part;
      const message = JSON.parse(body);
      if (request.headers.authorization !== 'Bearer upgrade-only-mcp') throw new Error('wrong fixture authority');
      if (message.id === undefined) { response.writeHead(202); response.end(); return; }
      const result = message.method === 'initialize' ? { protocolVersion: '2025-03-26', capabilities: { tools: {} }, serverInfo: { name: 'upgrade-fixture', version: '1' } }
        : message.method === 'tools/list' ? { tools: [{ name: 'get_issue', description: 'Unused qualification fixture', inputSchema: { type: 'object', properties: {} } }] }
        : (() => { throw new Error('upgrade must not invoke business tools'); })();
      response.writeHead(200, { 'Content-Type': 'application/json' }); response.end(JSON.stringify({ jsonrpc: '2.0', id: message.id, result })); return;
    }
    let body = '';
    for await (const part of request) body += part;
    const data = JSON.parse(body);
    if (request.headers.authorization !== 'Bearer upgrade-only-provider') throw new Error('fixture auth mismatch');
    const users = data.messages.filter(m => m.role === 'user').map(m => text(m.content));
    const current = users.at(-1);
    if (current === 'upgrade-continue' && (count !== 0 || !users.includes('upgrade-source-context'))) throw new Error('replay contacted provider or source context lost');
    if (!['upgrade-source-context', 'upgrade-continue'].includes(current)) throw new Error('unexpected model prompt');
    count++;
    response.writeHead(200, { 'Content-Type': 'text/event-stream' });
    for (const [delta, finish_reason] of [[{ role: 'assistant', content: 'retained upgrade context verified' }, null], [{}, 'stop']]) {
      response.write(`data: ${JSON.stringify({ id: 'upgrade', object: 'chat.completion.chunk', created: 1, model: 'upgrade', choices: [{ index: 0, delta, finish_reason }] })}\n\n`);
    }
    response.end('data: [DONE]\n\n');
  });
  await new Promise(resolve => server.listen(8845, '127.0.0.1', resolve));
  const models = { providers: { upgrade: { baseUrl: 'http://127.0.0.1:8845/v1', api: 'openai-completions', models: [{ id: 'controlled', name: 'Upgrade qualification', reasoning: false, input: ['text'], contextWindow: 32000, maxTokens: 1024, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }] } } };
  const { manager, disposition } = await openDurableSession(configuration.session);
  process.stdout.write(JSON.stringify({ type: 'omnigrex_session_ready', sessionId: manager.getSessionId(), disposition, capabilities: { history_replay: true, session_continuation: true, state_format: 'pi-session/v3' } }) + '\n');
  await run(configuration, manager, models);
} catch (error) {
  process.stderr.write(`Pi upgrade fixture failed: ${error.message}\n`);
  process.stdout.write('{"type":"omnigrex_configuration_error"}\n', () => process.exit(1));
}
