// Disposable controlled-provider diagnostic, never a production provider bridge.
import http from 'node:http';
import { spawn } from 'node:child_process';
import { writeFile } from 'node:fs/promises';

// Node readline also treats Unicode separators as record boundaries. Pi JSONL
// permits those characters inside strings; frame exclusively on LF here.
function lines(stream, onLine, onEnd = () => {}) {
  let pending = '';
  stream.setEncoding('utf8');
  stream.on('data', chunk => {
    pending += chunk;
    for (let end; (end = pending.indexOf('\n')) >= 0;) {
      const line = pending.slice(0, end).replace(/\r$/, '');
      pending = pending.slice(end + 1);
      onLine(line);
    }
  });
  stream.on('end', onEnd);
}

let mode = 'protocol-success';
let requests = 0;
const server = http.createServer(async (request, response) => {
  let content = '';
  for await (const part of request) content += part;
  JSON.parse(content);
  if (request.headers.authorization !== 'Bearer diagnostic-only-key') {
    response.writeHead(401); response.end(); return;
  }
  requests++;
  if (mode === 'protocol-hold' || mode === 'protocol-cancel' || mode === 'protocol-process-exit') return;
  const retry = mode === 'protocol-retry' || mode === 'protocol-retry-cancel';
  if (mode === 'protocol-provider-error' || (retry && requests === 1)) {
    response.writeHead(retry ? 503 : 400, { 'Content-Type': 'application/json' });
    response.end(JSON.stringify({ error: { message: 'controlled failure diagnostic-only-key', type: 'invalid_request_error' } }));
    return;
  }
  response.writeHead(200, { 'Content-Type': 'text/event-stream' });
  const chunk = (delta, finish_reason = null) => response.write(`data: ${JSON.stringify({
    id: 'diagnostic', object: 'chat.completion.chunk', created: 1, model: 'controlled',
    choices: [{ index: 0, delta, finish_reason }],
  })}\n\n`);
  chunk({ role: 'assistant' });
  chunk({ content: 'diagnostic-complete\u2028unicode\u2029safe' });
  chunk({}, 'stop');
  response.end('data: [DONE]\n\n');
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const agentDir = '/run/pi-agent';
await writeFile(`${agentDir}/auth.json`, JSON.stringify({ diagnostic: { type: 'api_key', key: 'diagnostic-only-key' } }), { mode: 0o600 });
const retryDelayMs = Number(process.env.OMNIGREX_PI_DIAGNOSTIC_RETRY_DELAY_MS ?? 10);
if (!Number.isInteger(retryDelayMs) || retryDelayMs < 10 || retryDelayMs > 30000) throw new Error('invalid diagnostic retry delay');
await writeFile(`${agentDir}/settings.json`, JSON.stringify({ retry: { enabled: true, maxRetries: 1, baseDelayMs: retryDelayMs }, compaction: { enabled: false } }), { mode: 0o600 });
await writeFile(`${agentDir}/models.json`, JSON.stringify({ providers: { diagnostic: {
  baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: 'openai-completions',
  models: [{ id: 'controlled', name: 'Controlled diagnostic', reasoning: false, input: ['text'], contextWindow: 32000, maxTokens: 1024,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }],
} } }), { mode: 0o600 });

const child = spawn('/usr/local/bin/node', ['/opt/omnigrex/pi/node_modules/@earendil-works/pi-coding-agent/dist/cli.js',
  '--mode', 'rpc', '--offline', '--no-approve', '--no-extensions', '--no-mcp', '--no-skills', '--no-prompt-templates', '--no-themes', '--no-context-files',
  '--session-dir', '/run/pi-agent/sessions', '--session-id', 'omnigrex-pi-diagnostic', '--provider', 'diagnostic', '--model', 'controlled', '--tools', 'read',
  '--system-prompt', 'Return the controlled diagnostic response without using tools.'], { stdio: ['pipe', 'pipe', 'pipe'] });
process.stderr.write('pi-diagnostic-stderr\n');
child.stderr.pipe(process.stderr);
lines(process.stdin, line => {
  const command = JSON.parse(line);
  if (command.type === 'prompt') { mode = command.message; requests = 0; }
  child.stdin.write(`${line}\n`);
}, () => child.stdin.end());
lines(child.stdout, line => {
  const record = JSON.parse(line);
  if (record.type === 'agent_settled' && mode === 'protocol-lost-terminal') return;
  process.stdout.write(`${line}\n`);
  if (record.type === 'response' && record.command === 'prompt' && record.success) {
    if (mode === 'protocol-malformed') process.stdout.write('diagnostic-only-key invalid-json\n');
    if (mode === 'protocol-process-exit') setTimeout(() => { child.kill('SIGKILL'); process.exit(86); }, 30);
  }
});
child.on('exit', code => { server.close(); process.exit(code ?? 1); });
process.on('SIGTERM', () => { child.kill('SIGTERM'); server.close(); });
