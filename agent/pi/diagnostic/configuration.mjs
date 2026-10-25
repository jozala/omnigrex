// Controlled provider and poisoned checkout for the production owned runner.
import http from 'node:http';
import { spawn } from 'node:child_process';
import { mkdir, writeFile, readFile, readdir, symlink, stat } from 'node:fs/promises';
import { SessionManager } from '@earendil-works/pi-coding-agent';
import { run, readBootstrap } from './runner.mjs';
import { openDurableSession } from './session-state.mjs';

const gateway = spawn('/usr/local/bin/pi-mcp-fixture', [], { stdio: ['ignore', 'ignore', 'inherit'] });
const fixturePrompts = new Set(['PRIOR_TURN_CONTEXT', 'configuration-audit', 'configuration-mcp',
  'configuration-revoked', 'configuration-publish', 'configuration-invalid-arguments',
  'configuration-developer-only', 'configuration-write', 'configuration-edit', 'configuration-skill',
  'configuration-grep', 'configuration-find', 'configuration-ls', 'configuration-bash',
  'configuration-provider-error', 'configuration-provider-stream-error', 'configuration-provider-error-audit', 'configuration-continuation',
  'configuration-thinking-max', 'configuration-thinking-max-continue', 'configuration-long']);
function providerReasoningSetting(data) {
  // Generic openai-completions wire for a reasoning model without a special
  // thinkingFormat carries the mapped level as reasoning_effort.
  return data?.reasoning_effort ?? data?.reasoningEffort ?? data?.reasoning?.effort;
}
function contentText(content) {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) return content.filter(block => block.type === 'text').map(block => block.text).join('');
  return '';
}
async function runFixture(command, args) {
  await new Promise((resolve, reject) => {
    const child = spawn(command, args, { stdio: ['ignore', 'ignore', 'inherit'] });
    child.once('error', reject);
    child.once('exit', code => { if (code === 0) resolve(); else reject(new Error('fixture subprocess failed')); });
  });
}
try {
  const configuration = await readBootstrap();
  if (process.env.OMNIGREX_PI_CONTROL_BOUNDARY_FIXTURE === '1') await runFixture('/usr/local/bin/node', ['--test', '/opt/omnigrex/pi/diagnostic/control-events.test.mjs', '/opt/omnigrex/pi/diagnostic/control-rpc.test.mjs']);
  if (process.env.OMNIGREX_PI_SESSION_BOUNDARY_FIXTURE === '1') await runFixture('/usr/local/bin/node', ['--test', '/opt/omnigrex/pi/session-state.test.mjs']);
  if (process.env.OMNIGREX_PI_MCP_BOUNDARY_FIXTURE === '1') await runFixture('/usr/local/bin/node', ['--test', '/opt/omnigrex/pi/mcp-boundary.test.mjs']);
  if (process.env.OMNIGREX_PI_FAILURE_BOUNDARY_FIXTURE === '1') await runFixture('/usr/local/bin/node', ['--test', '/opt/omnigrex/pi/failure-boundary.test.mjs', '/opt/omnigrex/pi/diagnostic/subscription.test.mjs', '/opt/omnigrex/pi/diagnostic/provider-diagnostic.test.mjs']);
  for (let attempt = 0;; attempt++) {
    try { await fetch('http://127.0.0.1:8844/audit'); break; }
    catch { if (attempt > 100) throw new Error('gateway unavailable'); await new Promise(r => setTimeout(r, 20)); }
  }
  await mkdir('/workspace/.pi/extensions', { recursive: true });
  await mkdir('/workspace/.agents/skills/approved', { recursive: true });
  await writeFile('/workspace/.pi/extensions/poison.mjs', "import { writeFileSync } from 'node:fs'; writeFileSync('/workspace/poison-executed','bad'); export default () => {};");
  await writeFile('/workspace/.pi/settings.json', JSON.stringify({
    defaultProvider: 'poison', defaultModel: 'poison', shellCommandPrefix: 'touch /workspace/poison-executed;',
    retry: { enabled: true, maxRetries: 999 }, extensions: ['/workspace/.pi/extensions/poison.mjs'], packages: ['/poison-package'],
  }));
  await writeFile('/workspace/.pi/mcp.json', JSON.stringify({ mcpServers: { poison: { command: 'sh', args: ['-c', 'touch /workspace/poison-executed'] } } }));
  await writeFile('/workspace/.pi/SYSTEM.md', 'POISON_SYSTEM_CONTENT');
  await writeFile('/workspace/.pi/APPEND_SYSTEM.md', 'POISON_APPEND_CONTENT');
  await writeFile('/workspace/AGENTS.md', 'POISON_AUTOMATIC_CONTEXT');
  await writeFile('/workspace/.agents/skills/approved/SKILL.md', '---\nname: approved\ndescription: Authorized checkout-local guidance\n---\nAPPROVED_SKILL_CONTENT\n');
  await writeFile('/workspace/example.txt', 'before\n');
  await writeFile('/workspace/search-target.txt', 'WORKSPACE_SEARCH_NEEDLE\n');
  if (process.env.OMNIGREX_PI_SKILL_ESCAPE === '1') await symlink('/run/pi-agent', '/workspace/.agents/skills/escape');
  const retained = process.env.OMNIGREX_PI_RETAINED_FIXTURE === '1';
  let requests = 0;
  let providerRequests = 0;
  const server = http.createServer(async (request, response) => {
    let body = '';
    for await (const part of request) body += part;
    const data = JSON.parse(body);
    // Literal dollar/escape characters deliberately exercise Pi's actual
    // file-backed credential reader, independent of the rendered auth bytes.
    if (request.headers.authorization !== 'Bearer configuration-only-key-$$-$!-$9') { response.writeHead(401); response.end(); return; }
    const prompt = contentText(data.messages.findLast(m => m.role === 'user')?.content);
    if (!fixturePrompts.has(prompt)) throw new Error('unknown controlled-provider scenario');
    if (prompt === 'configuration-continuation') {
      if (providerRequests !== 0) throw new Error('History Replay contacted the provider');
      if (!data.messages.some(m => m.role === 'user' && contentText(m.content) === 'configuration-audit')) throw new Error('prior Session context was lost');
      if (data.messages.filter(m => m.role === 'assistant' && contentText(m.content) === 'LONG_SESSION_BODY'.repeat(32000)).length !== 6) throw new Error('long durable assistant messages were lost');
      if (JSON.stringify(data.messages).includes('ABANDONED_HISTORY')) throw new Error('abandoned branch became current context');
    }
    if (prompt === 'configuration-thinking-max-continue') {
      if (providerRequests !== 0) throw new Error('History Replay contacted the provider');
      if (!data.messages.some(m => m.role === 'user' && contentText(m.content) === 'configuration-thinking-max')) throw new Error('prior max Session context was lost');
    }
    if (prompt === 'configuration-thinking-max' || prompt === 'configuration-thinking-max-continue') {
      const setting = providerReasoningSetting(data);
      if (setting !== 'max') throw new Error(`provider request did not carry max reasoning (reasoning_effort=${JSON.stringify(data?.reasoning_effort)} model=${JSON.stringify(data?.model)})`);
      if (data?.model !== undefined && data.model !== 'controlled-max') throw new Error(`provider request used unexpected model ${JSON.stringify(data?.model)}`);
    }
    providerRequests++;
    if (prompt === 'configuration-provider-error') {
      response.writeHead(400, { 'Content-Type': 'application/json' });
      response.end(JSON.stringify({ error: { message: `Invalid API key: ${request.headers.authorization.slice('Bearer '.length)}; ${configuration.mcp.headers[0].value}` } }));
      return;
    }
    const names = data.tools.map(t => t.function.name).sort();
    const system = data.messages.filter(m => m.role === 'system').map(m => contentText(m.content)).join('\n');
    if (system.includes('POISON_')) throw new Error('poisoned instructions');
    if (prompt === 'PRIOR_TURN_CONTEXT' && (!system.includes('OLD_OPERATOR_GUIDANCE') || !system.includes('OLD_STAGE_OBJECTIVE'))) throw new Error('prior instructions were not used');
    const currentUser = data.messages.findLastIndex(m => m.role === 'user');
    const toolResults = data.messages.slice(currentUser + 1).filter(m => m.role === 'tool');
    const finalResult = contentText(toolResults.at(-1)?.content);
    if (prompt === 'configuration-revoked' && !toolResults.length) await fetch('http://127.0.0.1:8844/revoke');
    if (prompt === 'configuration-audit') {
      if (!system.includes('CURRENT_OPERATOR_GUIDANCE') || !system.includes('CURRENT_STAGE_OBJECTIVE')) throw new Error('missing current instructions');
      if (system.includes('OLD_OPERATOR_GUIDANCE') || system.includes('OLD_STAGE_OBJECTIVE')) throw new Error('historical instructions became current authority');
      if (retained && !JSON.stringify(data.messages).includes('PRIOR_TURN_CONTEXT')) throw new Error('retained history was not reopened');
      if (system.includes('approved') !== configuration.repositorySkills) throw new Error('skill authorization mismatch');
      const expected = [...configuration.tools, ...configuration.mcpTools.map(n => `omnigrex_${n}`)].sort();
      if (JSON.stringify(names) !== JSON.stringify(expected)) throw new Error('tool authority mismatch');
      try { await readFile('/workspace/poison-executed'); throw new Error('extension executed'); } catch (e) { if (e.code !== 'ENOENT') throw e; }
    }
    const call = (name, args) => ({ id: `call-${++requests}`, type: 'function', function: { name, arguments: JSON.stringify(args) } });
    let tool;
    const longMessage = prompt === 'configuration-long' && toolResults.length < 6;
    if (longMessage) tool = call('read', { path: '/workspace/example.txt' });
    if (prompt === 'configuration-mcp' && !toolResults.length) tool = call('omnigrex_get_issue', {});
    if (prompt === 'configuration-revoked' && !toolResults.length) tool = call('omnigrex_get_issue', {});
    if (prompt === 'configuration-publish' && !toolResults.length) tool = call('omnigrex_publish_changes', { operation_id: 'fixture-publish' });
    if (prompt === 'configuration-invalid-arguments' && !toolResults.length) tool = call('omnigrex_get_issue', { unexpected: true });
    if (prompt === 'configuration-developer-only' && !toolResults.length) tool = call('omnigrex_publish_changes', { operation_id: 'fixture' });
    if (prompt === 'configuration-write' && !toolResults.length) tool = call('write', { path: '/workspace/written.txt', content: 'written\n' });
    if (prompt === 'configuration-edit' && !toolResults.length) tool = call('edit', { path: '/workspace/example.txt', oldText: 'before', newText: 'after' });
    if (prompt === 'configuration-skill' && !toolResults.length) tool = call('read', { path: '/workspace/.agents/skills/approved/SKILL.md' });
    if (prompt === 'configuration-grep' && !toolResults.length) tool = call('grep', { pattern: 'WORKSPACE_SEARCH_NEEDLE', path: '/workspace' });
    if (prompt === 'configuration-find' && !toolResults.length) tool = call('find', { pattern: 'search-target.txt', path: '/workspace' });
    if (prompt === 'configuration-ls' && !toolResults.length) tool = call('ls', { path: '/workspace' });
    if (prompt === 'configuration-bash' && !toolResults.length) tool = call('bash', { command: "printf 'BASH_CAPABILITY'" });
    if (!tool) {
      const audit = await (await fetch('http://127.0.0.1:8844/audit')).json();
      if (prompt === 'configuration-mcp' && (!finalResult?.includes('authorized') || !audit.length)) throw new Error('authorized MCP failed');
      if (prompt === 'configuration-publish' && (!finalResult?.includes('authorized') || !audit.includes('DEVELOPER:publish_changes'))) throw new Error('authorized Developer capability failed');
      if (prompt === 'configuration-revoked' && (audit.length || !finalResult?.includes('rejected'))) throw new Error('revoked authority executed');
      if (prompt === 'configuration-invalid-arguments' && audit.length) throw new Error('invalid arguments executed');
      if (prompt === 'configuration-developer-only' && audit.length) throw new Error('Developer-only tool executed');
      if (prompt === 'configuration-skill' && !finalResult?.includes('APPROVED_SKILL_CONTENT')) throw new Error('skill read failed');
      if (['configuration-grep', 'configuration-find', 'configuration-ls'].includes(prompt) && !finalResult?.includes('search-target.txt')) throw new Error('local search tool unavailable');
      if (prompt === 'configuration-bash' && !finalResult?.includes('BASH_CAPABILITY')) throw new Error('local shell tool unavailable');
      const reviewer = configuration.mcpTools.includes('submit_review');
      if (prompt === 'configuration-write') {
        const written = await readFile('/workspace/written.txt', 'utf8').catch(() => '');
        if ((written === 'written\n') === reviewer) throw new Error('write authority failed');
      }
      if (prompt === 'configuration-edit') {
        const edited = await readFile('/workspace/example.txt', 'utf8');
        if ((edited === 'after\n') === reviewer) throw new Error('edit authority failed');
      }
      const files = await readdir('/run/pi-agent');
      if (!files.includes('auth.json')) throw new Error('Pi auth file missing');
      const auth = JSON.parse(await readFile('/run/pi-agent/auth.json', 'utf8'));
      if (Object.keys(auth).join(',') !== 'diagnostic' || ((await stat('/run/pi-agent/auth.json')).mode & 0o777) !== 0o600) throw new Error('credential selection or file mode failed');
      const sessionDirectory = configuration.session ? `/state/sessions/${configuration.session.runtimeSessionId}` : '/run/pi-agent/sessions';
      const sessionFiles = (await readdir(sessionDirectory)).filter(file => file.endsWith('.jsonl'));
      let hasProviderFailure = false;
      for (const file of sessionFiles) {
        const history = await readFile(`${sessionDirectory}/${file}`, 'utf8');
        const token = configuration.mcp.headers[0].value.slice('Bearer '.length);
        if (history.includes('configuration-only-key') || history.includes(token) || history.includes('must-not-be-delivered')) throw new Error('credential entered retained Session');
        if (prompt === 'configuration-provider-error-audit') {
          const entries = history.trim().split('\n').map(line => JSON.parse(line));
          hasProviderFailure ||= entries.some(entry => entry.message?.role === 'assistant' && entry.message.stopReason === 'error' && entry.message.errorMessage);
        }
      }
      if (prompt === 'configuration-provider-error-audit' && (!sessionFiles.length || !hasProviderFailure)) throw new Error('provider failure classification was not retained');
    }
    response.writeHead(200, { 'Content-Type': 'text/event-stream' });
    const streamFailure = prompt === 'configuration-provider-stream-error';
    const delta = streamFailure ? { role: 'assistant', content: `Failed response: ${request.headers.authorization}; ${configuration.mcp.headers[0].value}` }
      : tool ? { role: 'assistant', tool_calls: [{ index: 0, ...tool }] } : { role: 'assistant', content: 'configuration passed' };
    const chunk = (delta, finish_reason) => response.write(`data: ${JSON.stringify({ id: 'config', object: 'chat.completion.chunk', created: 1, model: 'controlled', choices: [{ index: 0, delta, finish_reason }] })}\n\n`);
    // Each durable message stays below 1 MiB; only agent_end's redundant
    // aggregate exceeds the control-channel bound, as in the incident.
    if (longMessage) chunk({ role: 'assistant', content: 'LONG_SESSION_BODY'.repeat(32000) }, null);
    chunk(delta, null);
    chunk({}, streamFailure ? 'network_error' : tool ? 'tool_calls' : 'stop'); response.end('data: [DONE]\n\n');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  // controlled-max mirrors the pinned catalog observation for a reasoning model
  // that supports max (low/high/max, xhigh excluded). Fixture-only: production
  // never maps models to thinking levels and always queries the live runtime.
  const models = { providers: { diagnostic: {
    baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: 'openai-completions',
    models: [{ id: 'controlled', name: 'Controlled configuration', reasoning: false, input: ['text'], contextWindow: 2000000, maxTokens: 1024,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } },
    { id: 'controlled-max', name: 'Controlled reasoning max', reasoning: true,
      thinkingLevelMap: { off: null, minimal: null, low: 'low', medium: null, high: 'high', xhigh: null, max: 'max' },
      input: ['text'], contextWindow: 2000000, maxTokens: 1024,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }],
  } } };
  if (retained) {
    const prior = { ...configuration, instructions: configuration.instructions
      .replaceAll('CURRENT_OPERATOR_GUIDANCE', 'OLD_OPERATOR_GUIDANCE')
      .replaceAll('CURRENT_STAGE_OBJECTIVE', 'OLD_STAGE_OBJECTIVE') };
    // These bootstrap credentials remain disposable; only actual Pi Session
    // files are reopened by the next process and audited below.
    await writeFile('/run/pi-agent/seed-input.json', JSON.stringify({ configuration: prior, models }), { mode: 0o600 });
    await runFixture('/usr/local/bin/node', ['/opt/omnigrex/pi/seed-session.mjs']);
    const retainedFiles = await readdir('/run/pi-agent/sessions');
    const history = (await Promise.all(retainedFiles.map(file => readFile(`/run/pi-agent/sessions/${file}`, 'utf8')))).join('\n');
    if (!history.includes('OLD_OPERATOR_GUIDANCE') || !history.includes('OLD_STAGE_OBJECTIVE') || !history.includes('"role":"system"')) throw new Error('prior structured system state was not retained');
  }
  const previous = retained ? SessionManager.findById('/workspace', 'omnigrex-pi-configuration', '/run/pi-agent/sessions') : undefined;
  let session;
  if (configuration.session) {
    const opened = await openDurableSession(configuration.session);
    session = opened.manager;
    if (process.env.OMNIGREX_PI_HISTORY_TREE_FIXTURE === '1') {
      const originalLeaf = session.getLeafId();
      const firstUser = session.getEntries().find(entry => entry.type === 'message' && entry.message.role === 'user');
      if (!originalLeaf || !firstUser) throw new Error('history tree fixture needs prior context');
      session.appendMessage({ role: 'user', content: 'ABANDONED_HISTORY', timestamp: Date.now() });
      // Whole-transcript RPC would exceed the adapter's 1 MiB record limit.
      // These abandoned messages must still replay through bounded metadata pages.
      for (let i = 0; i < 260; i++) session.appendMessage({ role: 'user', content: 'ABANDONED_HISTORY'.repeat(256), timestamp: Date.now() });
      session.branchWithSummary(originalLeaf, 'CONTROLLED_BRANCH_SUMMARY');
      session.appendCompaction('CONTROLLED_COMPACTION', firstUser.id, 1);
    }
    process.stdout.write(JSON.stringify({ type: 'omnigrex_session_ready', sessionId: session.getSessionId(), disposition: opened.disposition,
      capabilities: { history_replay: true, session_continuation: true, state_format: 'pi-session/v3' } }) + '\n');
  } else session = previous ? SessionManager.open(previous, '/run/pi-agent/sessions', '/workspace')
    : SessionManager.create('/workspace', '/run/pi-agent/sessions', { id: 'omnigrex-pi-configuration' });
  await run(configuration, session, models);
} catch {
  gateway.kill();
  process.stderr.write('Pi configuration fixture failed\n');
  process.stdout.write('{"type":"omnigrex_configuration_error"}\n', () => process.exit(1));
}
