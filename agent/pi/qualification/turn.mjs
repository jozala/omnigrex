// Qualification only: replace the HTTP model catalog, retaining production
// bootstrap, durable preparation, resources, RPC, permissions and MCP bridge.
import http from 'node:http';
import { readBootstrap, run } from '../runner.mjs';
import { openDurableSession } from '../session-state.mjs';

const text = value => typeof value === 'string' ? value : Array.isArray(value) ? value.filter(b => b.type === 'text').map(b => b.text).join('') : '';
try {
  const configuration = await readBootstrap();
  let requestCount = 0;
  const server = http.createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    const data = JSON.parse(body);
    if (request.headers.authorization !== 'Bearer workflow-only-provider-key') throw new Error('fixture auth mismatch');
    const userIndex = data.messages.findLastIndex(m => m.role === 'user');
    const envelope = JSON.parse(text(data.messages[userIndex].content));
    if (!envelope.agent_turn_id || !envelope.agent_session_id) throw new Error('missing production envelope');
    const role = envelope.triggering_event.role;
    const results = data.messages.slice(userIndex + 1).filter(m => m.role === 'tool');
    const step = results.length;
    const names = data.tools.map(t => t.function.name);
    if (role === 'REVIEWER' && (names.includes('write') || names.includes('edit') || names.includes('omnigrex_publish_changes'))) throw new Error('Reviewer authority expanded');
    const result = index => JSON.parse(text(results[index].content));
    let name, args;
    if (role === 'DEVELOPER') {
      if (step === 0) { name = 'bash'; args = { command: "printf 'one\\n' > feature.txt; git add feature.txt; git commit -m 'Add first change'; printf 'two\\n' >> feature.txt; git add feature.txt; git commit -m 'Complete change'" }; }
      if (step === 1) { name = 'omnigrex_publish_changes'; args = { operation_id: 'controlled-publish' }; }
      if (step === 2) { name = 'omnigrex_open_pr'; args = { operation_id: 'controlled-open', title: 'Controlled Pi Change Proposal', body: 'Ready for review.' }; }
      if (step === 3) { name = 'omnigrex_request_review'; args = { operation_id: 'controlled-handoff', head_sha: result(1).head_sha, summary: 'Two committed changes are ready for review.' }; }
    } else if (role === 'REVIEWER') {
      if (step === 0) { name = 'omnigrex_get_pull_request'; args = {}; }
      if (step === 1) { name = 'omnigrex_submit_review'; args = { operation_id: 'controlled-review', event: 'APPROVE', body: 'The controlled changes are correct.', comments: [] }; }
    } else throw new Error('unexpected Workflow Role');
    if (results.some(m => text(m.content).includes('"isError":true') || text(m.content).includes('operation was rejected'))) throw new Error('controlled capability failed');
    response.writeHead(200, { 'Content-Type': 'text/event-stream' });
    const tool = name ? { id: `controlled-${++requestCount}`, type: 'function', function: { name, arguments: JSON.stringify(args) } } : null;
    const chunk = (delta, finish_reason) => response.write(`data: ${JSON.stringify({ id: 'controlled-workflow', object: 'chat.completion.chunk', created: 1, model: 'controlled', choices: [{ index: 0, delta, finish_reason }] })}\n\n`);
    chunk(tool ? { role: 'assistant', tool_calls: [{ index: 0, ...tool }] } : { role: 'assistant', content: 'Controlled Turn complete.' }, null);
    chunk({}, tool ? 'tool_calls' : 'stop'); response.end('data: [DONE]\n\n');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const models = { providers: { controlled: { baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: 'openai-completions', models: [{ id: 'workflow', name: 'Controlled workflow', reasoning: false, input: ['text'], contextWindow: 32000, maxTokens: 1024, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }] } } };
  const { manager, disposition } = await openDurableSession(configuration.session);
  process.stdout.write(JSON.stringify({ type: 'omnigrex_session_ready', sessionId: manager.getSessionId(), disposition, capabilities: { history_replay: true, session_continuation: true, state_format: 'pi-session/v3' } }) + '\n');
  await run(configuration, manager, models);
} catch {
  process.stdout.write('{"type":"omnigrex_configuration_error"}\n', () => process.exit(1));
}
