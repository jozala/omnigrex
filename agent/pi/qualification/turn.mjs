// Qualification only: replace the HTTP model catalog, retaining production
// bootstrap, durable preparation, resources, RPC, permissions and MCP bridge.
import http from 'node:http';
import { stat, writeFile } from 'node:fs/promises';
import { readBootstrap, run } from '../runner.mjs';
import { openDurableSession } from '../session-state.mjs';

const text = value => typeof value === 'string' ? value : Array.isArray(value) ? value.filter(b => b.type === 'text').map(b => b.text).join('') : '';
try {
  const configuration = await readBootstrap();
  const fault = process.env.OMNIGREX_PI_WORKFLOW_FAULT ?? '';
  const longFile = '/run/pi-agent/long-fixture.txt';
  await writeFile(longFile, 'Controlled long-Turn tool result.');
  let requestCount = 0;
  const server = http.createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    const data = JSON.parse(body);
    if (request.headers.authorization !== 'Bearer workflow-only-provider-key') throw new Error('fixture auth mismatch');
    const userIndex = data.messages.findLastIndex(m => m.role === 'user');
    const envelope = JSON.parse(text(data.messages[userIndex].content));
    if (!envelope.agent_turn_id || !envelope.agent_session_id) throw new Error('missing production envelope');
    const tools = '/home/omnigrex/.local/share/omnigrex-tool-data';
    const expectedPaths = { TMPDIR: `${tools}/turn/${envelope.agent_turn_id}/build-tmp`, GOTMPDIR: `${tools}/turn/${envelope.agent_turn_id}/build-tmp`, GOCACHE: `${tools}/assignment/go-build-cache`, GOPATH: `${tools}/assignment/go-modules` };
    for (const [name, path] of Object.entries(expectedPaths)) {
      if (process.env[name] !== path || !(await stat(path)).isDirectory()) throw new Error('admitted disk-backed path was not preserved');
    }
    const role = envelope.triggering_event.role;
    if (['restart-during-prompt', 'lost-transport', 'lease-expiry', 'cleanup-failure'].includes(fault)) return;
    if (fault === 'runtime-exit-137') { process.exit(137); }
    if (fault === 'resource-exhaustion') {
      // Touch and retain real allocations beyond the bounded environment's
      // memory budget. A signal/code alone is never asserted to be OOM proof.
      const allocations = [];
      setInterval(() => allocations.push(Buffer.alloc(16 * 1024 * 1024, 1)), 10);
      return;
    }
    const results = data.messages.slice(userIndex + 1).filter(m => m.role === 'tool');
    const step = results.length;
    const names = (data.tools ?? []).map(t => t.function.name);
    if (role === 'REVIEWER' && (names.includes('write') || names.includes('edit') || names.includes('omnigrex_publish_changes'))) throw new Error('Reviewer authority expanded');
    const result = index => JSON.parse(text(results[index].content));
    let name, args;
    if (names.length === 0) {
      const system = data.messages.filter(m => m.role === 'system').map(m => text(m.content)).join('\n');
      if (!system.includes('An incomplete outcome is permitted')) throw new Error('missing reserved summary instruction');
    } else if (fault === 'concurrent-turns') {
      const notify = phase => fetch(`${process.env.OMNIGREX_PI_WORKFLOW_CONTROL_URL}?turn=${envelope.agent_turn_id}&phase=${phase}&issue=${envelope.issue_number}`);
      if (step > 0) {
        if (result(step - 1).number !== envelope.issue_number) throw new Error('cross-Turn MCP authority');
        await notify(step === 1 ? 'first' : 'second');
      }
      if (step > 1) return;
      if (step === 1) await notify('continue');
      name = 'omnigrex_get_issue'; args = {};
    } else if ((fault === 'clarification' || fault === 'clarification-restart')) {
      const rounds = (envelope.clarification_history ?? []).filter(r => r.participant_id === envelope.agent_participant_id);
      const input = envelope.human_input ?? [];
      const prefix = rounds.length > 0 ? 1 : 0;
      if (step === 0 && prefix) {
        if (!input.length || input.some(i => !i.id || !i.actor?.id || !i.source?.id)) throw new Error('missing correlated human input');
        const round = rounds.at(-1);
        name = 'omnigrex_resolve_clarification'; args = { operation_id: `resolve-${round.id}`, round_id: round.id, resolutions: [{question_id: round.questions[0].id, response_ids: input.map(i=>i.id), explanation: 'The explicit fixture answer resolves this question.'}] };
      } else if (rounds.length < 2 && step === prefix) {
        name = 'omnigrex_request_clarification'; args = { operation_id: `ask-${rounds.length+1}`, questions: [{id:`${role}-Q${rounds.length+1}`,body:'Which fixture option should govern?'}] };
      } else if (rounds.length >= 2) {
        const normal = step-prefix;
        if (role === 'DEVELOPER') {
          if (normal === 0) { name='bash'; args={command: "printf 'clarified\\n' > feature.txt; git add feature.txt; git commit -m 'Implement clarified choice'"}; }
          if (normal === 1) { name='omnigrex_publish_changes'; args={operation_id:'controlled-publish'}; }
          if (normal === 2) { name='omnigrex_open_pr'; args={operation_id:'controlled-open',title:'Clarified Change Proposal',body:'Two clarification rounds completed.'}; }
          if (normal === 3) { name='omnigrex_request_review'; args={operation_id:'controlled-handoff',summary:'Two clarified choices verified.'}; }
        } else {
          if (normal === 0) { name='omnigrex_get_check_runs'; args={}; }
          if (normal === 1) { name='omnigrex_get_pull_request'; args={}; }
          if (normal === 2) { name='omnigrex_submit_review'; args={operation_id:'controlled-review',event:'APPROVE',body:'Current code reviewed after clarification.',comments:[]}; }
        }
      }
    } else if (fault === 'long-blocked') {
      if (step === 0) { name = 'omnigrex_report_blocked'; args = { operation_id: 'controlled-blocked', reason: 'Controlled prerequisite unavailable', details: 'Synthetic long-Turn blocked completion.' }; }
    } else if (fault === 'default-branch-refresh') {
      if (!names.includes('omnigrex_refresh_default_branch')) throw new Error('refresh missing from production MCP discovery');
      const check = index => {
        const snapshot = result(index);
        if (snapshot.status !== 'current' || !/^[0-9a-f]{40}$/.test(snapshot.sha) || !/^refs\/omnigrex\/default\/[0-9a-f]{64}$/.test(snapshot.ref)) throw new Error('missing fresh snapshot');
        return `git cat-file -e ${snapshot.sha}^{commit} && git merge-base HEAD ${snapshot.ref} && git merge-tree --write-tree HEAD ${snapshot.ref}`;
      };
      if (role === 'DEVELOPER') {
        if (step === 0) { name = 'bash'; args = { command: "printf 'one\n' > feature.txt; git add feature.txt; git commit -m 'Controlled change'" }; }
        if (step === 1) { name = 'omnigrex_refresh_default_branch'; args = { operation_id: 'initial-base' }; }
        if (step === 2) { name = 'bash'; args = { command: `${check(1)} && git merge --no-edit ${result(1).ref} && printf SNAPSHOT_VERIFIED` }; }
        if (step === 3) { if (!text(results[2].content).includes('SNAPSHOT_VERIFIED')) throw new Error('Developer local verification failed'); name = 'omnigrex_publish_changes'; args = { operation_id: 'controlled-publish' }; }
        if (step === 4) { name = 'omnigrex_open_pr'; args = { operation_id: 'controlled-open', title: 'Controlled refresh', body: 'Verified current default branch.' }; }
        if (step === 5) { name = 'omnigrex_refresh_default_branch'; args = { operation_id: 'final-base' }; }
        if (step === 6) { name = 'bash'; args = { command: `${check(5)} && printf SNAPSHOT_VERIFIED` }; }
        if (step === 7) { if (!text(results[6].content).includes('SNAPSHOT_VERIFIED')) throw new Error('final local verification failed'); name = 'omnigrex_request_review'; args = { operation_id: 'controlled-handoff', summary: 'Acquired and locally checked current default snapshot.' }; }
      } else {
        if (step === 0) { name = 'omnigrex_refresh_default_branch'; args = { operation_id: 'review-base' }; }
        if (step === 1) { name = 'bash'; args = { command: `test "$(git rev-parse HEAD)" = "${envelope.current_head_sha}" && ${check(0)} && test -z "$(git status --porcelain)" && printf SNAPSHOT_VERIFIED` }; }
        if (step === 2) { if (!text(results[1].content).includes('SNAPSHOT_VERIFIED')) throw new Error('Reviewer local verification failed'); name = 'omnigrex_submit_review'; args = { operation_id: 'controlled-review', event: 'APPROVE', body: 'Current default snapshot verified locally.', comments: [] }; }
      }
    } else if (role === 'DEVELOPER') {
      if (step === 0) { name = 'bash'; args = { command: "printf 'one\\n' > feature.txt; git add feature.txt; git commit -m 'Add first change'; printf 'two\\n' >> feature.txt; git add feature.txt; git commit -m 'Complete change'" }; }
      if (step === 1) { name = 'omnigrex_publish_changes'; args = { operation_id: 'controlled-publish' }; }
      if (step === 2) { name = 'omnigrex_open_pr'; args = { operation_id: 'controlled-open', title: 'Controlled Pi Change Proposal', body: 'Ready for review.' }; }
      if (step === 3) { name = 'omnigrex_request_review'; args = { operation_id: 'controlled-handoff', head_sha: result(1).head_sha, summary: 'Two committed changes are ready for review.' }; }
    } else if (role === 'REVIEWER') {
      if (step === 0) { name = 'omnigrex_get_check_runs'; args = {}; }
      if (step === 1) {
        const evidence = result(0);
        if (evidence.required_policy?.availability !== 'available' || evidence.targets?.[0]?.observation?.statuses?.[0]?.state !== 'failure' || evidence.head_sha !== envelope.current_head_sha) throw new Error('missing scoped required-CI evidence');
        name = 'omnigrex_get_pull_request'; args = {};
      }
      if (step === 2) { name = 'omnigrex_submit_review'; args = { operation_id: 'controlled-review', event: 'APPROVE', body: 'The controlled changes are correct.', comments: [] }; }
    } else throw new Error('unexpected Workflow Role');
    if (results.some(m => text(m.content).includes('"isError":true') || text(m.content).includes('Omnigrex MCP operation failed or was rejected'))) {
      if (!fault) throw new Error('controlled capability failed');
      name = undefined; args = undefined;
    }
    const paddingCalls = data.messages.slice(userIndex + 1).filter(m => m.role === 'assistant')
      .flatMap(m => m.tool_calls ?? []).filter(t => t.function.name === 'read' && JSON.parse(t.function.arguments).path === longFile);
    const longMessage = ['', 'long-blocked'].includes(fault) && !name && paddingCalls.length < 6;
    if (longMessage) { name = 'read'; args = { path: longFile }; }
    response.writeHead(200, { 'Content-Type': 'text/event-stream' });
    const tool = name ? { id: `controlled-${++requestCount}`, type: 'function', function: { name, arguments: JSON.stringify(args) } } : null;
    const chunk = (delta, finish_reason) => response.write(`data: ${JSON.stringify({ id: 'controlled-workflow', object: 'chat.completion.chunk', created: 1, model: 'controlled', choices: [{ index: 0, delta, finish_reason }] })}\n\n`);
    // Several individually bounded assistant messages produce a multi-megabyte
    // agent_end without violating the separate durable Session entry limit.
    if (longMessage) chunk({ role: 'assistant', content: 'LONG_WORKFLOW_BODY'.repeat(32000) }, null);
    chunk(tool ? { role: 'assistant', tool_calls: [{ index: 0, ...tool }] } : { role: 'assistant', content: 'Controlled Turn complete.' }, null);
    chunk({}, tool ? 'tool_calls' : 'stop'); response.end('data: [DONE]\n\n');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const models = { providers: { controlled: { baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: 'openai-completions', models: [{ id: 'workflow', name: 'Controlled workflow', reasoning: false, input: ['text'], contextWindow: 2000000, maxTokens: 1024, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }] } } };
  const { manager, disposition } = await openDurableSession(configuration.session);
  process.stdout.write(JSON.stringify({ type: 'omnigrex_session_ready', sessionId: manager.getSessionId(), disposition, capabilities: { history_replay: true, session_continuation: true, state_format: 'pi-session/v3' } }) + '\n');
  await run(configuration, manager, models);
} catch {
  process.stdout.write('{"type":"omnigrex_configuration_error"}\n', () => process.exit(1));
}
