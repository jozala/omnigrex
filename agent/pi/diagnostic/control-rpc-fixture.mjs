// Disposable controlled-provider fixture: real pinned SDK, stock RPC machinery,
// owned event view, and a Session file reopened by a fresh Node process.
import http from 'node:http';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { createAgentSessionRuntime, createAgentSessionServices, createAgentSessionFromServices,
  SettingsManager, runRpcMode } from '@earendil-works/pi-coding-agent';
import { controlRuntime } from '../control-events.mjs';
import { openDurableSession } from '../session-state.mjs';

const [root, mode, continued] = process.argv.slice(2);
const body = 'LONG_RPC_BODY'.repeat(40000);
const file = join(root, 'read-fixture.txt');
await writeFile(file, 'Controlled tool result.');
let attempts = 0;
const server = http.createServer(async (request, response) => {
  let raw = '';
  for await (const chunk of request) raw += chunk;
  const data = JSON.parse(raw);
  if (continued && data.messages.filter(message => message.role === 'assistant' && message.content === body).length !== 6) {
    response.writeHead(400); response.end(); return;
  }
  if (mode === 'retry' && ++attempts === 1) {
    response.writeHead(503, { 'Content-Type': 'application/json' });
    response.end(JSON.stringify({ error: { message: 'controlled temporary failure' } })); return;
  }
  response.writeHead(200, { 'Content-Type': 'text/event-stream' });
  const chunk = (delta, finish_reason = null) => response.write(`data: ${JSON.stringify({ id: 'control-rpc', object: 'chat.completion.chunk', created: 1, model: 'controlled', choices: [{ index: 0, delta, finish_reason }] })}\n\n`);
  chunk({ role: 'assistant' });
  const results = data.messages.slice(data.messages.findLastIndex(m => m.role === 'user') + 1).filter(m => m.role === 'tool');
  const longMessage = !continued && results.length < 6;
  if (longMessage) {
    chunk({ content: body });
    chunk({ tool_calls: [{ index: 0, id: `read-${results.length}`, type: 'function', function: { name: 'read', arguments: JSON.stringify({ path: file }) } }] });
    chunk({}, 'tool_calls');
    response.end('data: [DONE]\n\n'); return;
  }
  chunk({ content: continued ? 'continued with complete history' : 'controlled completion' });
  if (mode === 'cancel') return; // Real RPC abort must stop the pending stream.
  chunk({}, mode === 'error' ? 'network_error' : 'stop');
  response.end('data: [DONE]\n\n');
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const agentDir = join(root, 'agent');
await mkdir(agentDir, { recursive: true });
await writeFile(join(agentDir, 'auth.json'), JSON.stringify({ diagnostic: { type: 'api_key', key: 'controlled-only' } }));
await writeFile(join(agentDir, 'models.json'), JSON.stringify({ providers: { diagnostic: {
  baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: 'openai-completions',
  models: [{ id: 'controlled', name: 'Controlled', reasoning: false, input: ['text'], contextWindow: 2000000, maxTokens: 1024,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }],
} } }));
const id = '11111111-1111-4111-8111-111111111111';
const { manager } = await openDurableSession({ agentSessionId: id, runtimeSessionId: id, mustExist: Boolean(continued) }, root);
// Publish the path out of band for the parent; no transcript on stdout.
await writeFile(join(root, 'session-path'), manager.getSessionFile());
const settingsManager = SettingsManager.inMemory({ retry: { enabled: mode === 'retry', maxRetries: 1, baseDelayMs: 10 },
  compaction: { enabled: false }, cacheWarming: 'off', enableAnalytics: false, enableInstallTelemetry: false }, { projectTrusted: false });
const runtime = await createAgentSessionRuntime(async ({ sessionManager }) => {
  const services = await createAgentSessionServices({ cwd: '/workspace', agentDir, settingsManager,
    resourceLoaderOptions: { noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true, noContextFiles: true,
      disabledBuiltinExtensions: ['mcp'], systemPromptOverride: () => 'Controlled qualification only' } });
  const model = services.modelRuntime.getModel('diagnostic', 'controlled');
  const session = await createAgentSessionFromServices({ services, sessionManager, model, tools: ['read'] });
  return { ...session, services, diagnostics: services.diagnostics };
}, { cwd: '/workspace', agentDir, sessionManager: manager });
await runRpcMode(controlRuntime(runtime));
