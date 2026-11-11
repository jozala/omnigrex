// Qualification only: replace the HTTP model catalog, retaining production
// bootstrap, durable preparation, resources, RPC, permissions and MCP bridge.
import http from 'node:http';
import { isDeepStrictEqual } from 'node:util';
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
    const toolStep = results.length;
    const step = toolStep;
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
    } else if (fault === 'planning') {
      if(role!=='PLANNER'||names.some(n=>['omnigrex_publish_changes','omnigrex_open_pr','omnigrex_submit_review','omnigrex_request_review'].includes(n)))throw new Error('planning authority escaped');
      const system=data.messages.filter(m=>m.role==='system').map(m=>text(m.content)).join('\n');
      if(!system.includes('omnigrex-discovery/v2'))throw new Error('discovery missing on continuation');
      const rounds=(envelope.clarification_history??[]).filter(r=>r.participant_id===envelope.agent_participant_id);
      const input=envelope.human_input??[];
      const decisions=[...(envelope.human_input_history??[]),...input].filter(i=>i.kind!=='confirm-plan');
      const confirmation=input.find(i=>i.kind==='confirm-plan');
      const amended=input.some(i=>i.kind==='amend');
      const calls=[];
      const add=(name,args)=>calls.push(()=>({name,args:typeof args==='function'?args():args}));
      const get=i=>JSON.parse(text(results[i].content));
      const expectRejection=(first,second,reason)=>{
        const a=get(first),b=get(second);
        if(a.publication!=='not_applied'||a.reason!==reason||a.revision_id||a.source||!isDeepStrictEqual(a,b))throw new Error('non-publication lost through Pi or replay');
        return a;
      };
      if(input.length)add('omnigrex_assess_human_input',{operation_id:`assess-planning-input-${envelope.agent_turn_id}`,assessments:input.map(i=>({input_id:i.id,evidence_ids:i.kind==='answer'&&rounds.length===2?[decisions[0].id,i.id]:[i.id],...(i.kind==='answer'&&rounds.length===2?{corrects:[decisions[0].id]}:{}),disposition:i.kind==='confirm-plan'?'confirmed':'changed',explanation:i.kind==='confirm-plan'?'Exact published pair accepted without changes.':'Explicit fixture direction considered; retention changes, audience and selection remain unchanged unless amended.'}))});
      const resolve=(operation,round,resolutions)=>add('omnigrex_resolve_clarification',{operation_id:operation,round_id:round.id,resolutions});
      const resolution=(question,ids,explanation)=>({question_id:question,response_ids:ids,explanation});
      if(confirmation){
        const base=calls.length;
        add('omnigrex_refresh_default_branch',{operation_id:'planning-final-base'});
        add('bash',()=>({command:`git cat-file -e ${get(base).sha}^{commit} && git show ${get(base).sha}:README.md`}));
        add('omnigrex_complete_planning',()=>({operation_id:'planning-ready',revision_id:confirmation.target,repository_commit:get(base).sha,evidence:'README.md remains a minimal repository. Ephemeral local checklist requires no retention schema or external integration. All delivered human choices remain applicable.',material_decisions_resolved:true}));
      }else if(rounds.length===0){
        add('omnigrex_request_clarification',{operation_id:'planning-ask-1',questions:[{id:'Q1',body:'Target small teams without external accounts?'},{id:'Q1-selection',body:'Keep selected checklist steps explicit?'}]});
      }else if(rounds.length===1){
        resolve('resolve-first',rounds[0],[resolution('Q1',input.map(i=>i.id),'Small teams without accounts.'),resolution('Q1-selection',input.map(i=>i.id),'Explicit selected steps accepted.')]);
        add('omnigrex_request_clarification',{operation_id:'planning-ask-2',questions:[{id:'Q2',body:'Retain onboarding status? I recommend persistence.'}]});
      }else if(rounds.length===2){
        resolve('reaffirm-audience',rounds[0],[resolution('Q1',decisions.map(i=>i.id),'Audience remains small teams without accounts after the correction. Selection still needs explicit reconciliation.')]);
        add('omnigrex_request_clarification',{operation_id:'planning-ask-3',questions:[{id:'Q2-v2',replaces:'Q2',body:'Use ephemeral local status with no retained personal data instead?'}]});
      }else{
        if(amended){
          resolve('reinterpret-audience',rounds[0],[resolution('Q1',input.map(i=>i.id),'The amendment replaces the audience: users without repository access. Use the standalone operator CLI from steering.')]);
        }else{
          const invalid=calls.length;
          const bad={operation_id:'invalid-resolution-evidence',round_id:rounds.at(-1).id,resolutions:[resolution('Q2-v2',[decisions[1].id,...input.map(i=>i.id)],'Deliberately invalid older-question evidence.')]};
          add('omnigrex_resolve_clarification',bad);add('omnigrex_resolve_clarification',bad);
          calls.push(()=>{
            const a=get(invalid),b=get(invalid+1);
            if(a.recorded!==false||a.application!=='not_applied'||a.reason!=='response_not_valid_for_question'||!isDeepStrictEqual(a,b))throw new Error('resolution rejection masked by Pi');
            return {name:'omnigrex_resolve_clarification',args:{operation_id:'resolve-revised',round_id:rounds.at(-1).id,resolutions:[resolution('Q2-v2',input.map(i=>i.id),'Accept ephemeral local status without retention.')]}};
          });
        }
        const base=calls.length;
        add('omnigrex_refresh_default_branch',{operation_id:`planning-base-${amended?'amended':'initial'}`});
        const content=()=>({title:'Small-team onboarding',summary:'An ephemeral local checklist without accounts or personal-data retention.',unresolved_decisions:[],status:'proposed',changes:'Replace retention with the human choice: ephemeral local status.',requirements_brief:amended?'Users without repository access need a standalone ephemeral checklist. No retained personal data, repository permissions or external accounts. Reset discards status. Non-goals: deployment and implementation. Material unresolved questions: none.':'Small teams need ephemeral onboarding guidance. Required: selected checklist steps, no retained personal data or external accounts. Verify completion and reset. Non-goals: deployment and implementation. Material unresolved questions: none.',technical_plan:(amended?'Use a standalone operator CLI, not a repository plugin. ':'')+'README.md is the repository baseline. Separate transient status, explicit selected steps and tests; no database migration. Verify reset discards status. If persistence becomes necessary, clarify again. Names are suggestions.',investigated_commit:get(base).sha,evidence:['README.md at the acquired default commit'],decision_response_ids:decisions.map(i=>i.id)});
        const attempt=calls.length;
        const pair=()=>({operation_id:`planning-pair-${amended?'amended':'initial'}`,content:content()});
        add('omnigrex_publish_plan_checkpoint',pair);add('omnigrex_publish_plan_checkpoint',pair);
        if(!amended){
          calls.push(()=>{expectRejection(attempt,attempt+1,'late_input');return {name:'omnigrex_request_clarification',args:{operation_id:'drain-late-input',questions:[]}};});
        }else{
          calls.push(()=>{
            const rejected=expectRejection(attempt,attempt+1,'historical_decisions_unresolved');
            if(rejected.next_action!=='reconcile_historical_decisions'||rejected.decisions.length!==1||rejected.decisions[0].question_id!=='Q1-selection'||rejected.decisions[0].round_reference!==rounds[0].reference||!rejected.decisions[0].correction_references.includes(decisions[1].reference))throw new Error('historical prerequisite missing');
            return {name:'omnigrex_resolve_clarification',args:{operation_id:'reaffirm-selection',round_id:rounds[0].id,resolutions:[resolution('Q1-selection',decisions.filter(i=>i.id===decisions[0].id||i.id===decisions[1].id||i.kind==='amend').map(i=>i.id),'Original and correction evidence leave selected checklist steps unchanged. Explicitly reaffirm this historical interpretation without another human answer.')]}};
          });
          const published=calls.length;
          add('omnigrex_publish_plan_checkpoint',()=>({operation_id:'planning-pair-reconciled',content:content()}));
          calls.push(()=>{if(!get(published).revision_id||get(published).publication==='not_applied')throw new Error('expected complete published pair');return {name:'omnigrex_request_clarification',args:{operation_id:'await-confirmation',questions:[]}};});
        }
      }
      if(toolStep<calls.length)({name,args}=calls[toolStep]());
    } else if ((fault === 'clarification' || fault === 'clarification-restart')) {
      // Fetch bounded projected records through the real production MCP bridge.
      // Historical fixture bodies need not be re-read to interpret this new answer.
      const projected = envelope.collaboration_projection;
      let fetchRequest;
      let fetched = 0;
      const collections = {};
      if (projected?.retrieval_required) {
        for (const collection of ['human_input', 'clarification_history']) {
          collections[collection] = [];
          for (let index = 0; index < projected.counts[collection]; index++) {
            let content = '', offset = 0;
            while (true) {
              if (fetched >= results.length) { fetchRequest = { collection, index, offset }; break; }
              const page = result(fetched++);
              if (page.index !== index || typeof page.content !== 'string') throw new Error('invalid collaboration retrieval');
              content += page.content;
              if (page.next_offset === undefined) break;
              offset = page.next_offset;
            }
            if (fetchRequest) break;
            collections[collection].push(JSON.parse(content));
          }
          if (fetchRequest) break;
        }
      }
      const rounds = (collections.clarification_history ?? envelope.clarification_history ?? []).filter(r => r.participant_id === envelope.agent_participant_id);
      const input = collections.human_input ?? envelope.human_input ?? [];
      const actionStep = step - fetched - (input.length?1:0);
      const prefix = rounds.length > 0 ? 1 : 0;
      if (fetchRequest) { name = 'omnigrex_get_collaboration_record'; args = fetchRequest; }
      else if(step===fetched && input.length){name='omnigrex_assess_human_input';args={operation_id:`assess-clarification-input-${envelope.agent_turn_id}`,assessments:input.map(i=>({input_id:i.id,evidence_ids:[i.id],disposition:'changed',explanation:'Explicit fixture answer considered for this round.'}))};}
      else if (actionStep === 0 && prefix) {
        if (!input.length || input.some(i => !i.id || !i.actor?.id || !i.source?.id)) throw new Error('missing correlated human input');
        const round = rounds.at(-1);
        name = 'omnigrex_resolve_clarification'; args = { operation_id: `resolve-${round.id}`, round_id: round.id, resolutions: [{question_id: round.questions[0].id, response_ids: input.map(i=>i.id), explanation: 'The explicit fixture answer resolves this question.'}] };
      } else if (rounds.length < 2 && actionStep === prefix) {
        name = 'omnigrex_request_clarification'; args = { operation_id: `ask-${rounds.length+1}`, questions: [{id:`${role}-Q${rounds.length+1}`,body:'Which fixture option should govern?'}] };
      } else if (rounds.length >= 2) {
        const normal = actionStep-prefix;
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
    } else if (fault === 'no-pr-target') {
      if (envelope.schema_version !== 2 || envelope.triggering_event.turn_purpose !== 'INITIAL_DEVELOPMENT' || role !== 'DEVELOPER' || envelope.pull_request_context?.status !== 'none' || envelope.pull_request_number !== undefined) throw new Error('missing explicit initial absence');
      const reads = ['get_pull_request','list_pull_request_reviews','list_review_threads','get_handoff'];
      for (let i=0; i<Math.min(step,4); i++) {
        if (result(i)?.status !== 'no_pr_target') throw new Error('absence was not a successful scoped response');
      }
      if (step < 4) { name = `omnigrex_${reads[step]}`; args = {}; }
      if (step === 4) { name = 'bash'; args = {command: "printf 'implemented\\n' > feature.txt; git add feature.txt; git commit -m 'Implement after explicit absence'"}; }
      if (step === 5) { name = 'omnigrex_publish_changes'; args = {operation_id:'controlled-publish'}; }
      if (step === 6) { if (!result(5).head) throw new Error('publication failed'); name='omnigrex_open_pr'; args={operation_id:'controlled-open',title:'Explicit absence continuation',body:'Scoped absence is normal.'}; }
      if (step >= 7 && step < 11) {
        if (step > 7 && result(step-1)?.status === 'no_pr_target') throw new Error('PR target did not advance');
        name=`omnigrex_${reads[step-7]}`; args={};
      }
      if (step === 11) {
        if (result(7).number !== result(6).number || result(8).head_sha !== result(5).head || result(9).head_sha !== result(5).head || result(10) !== null) throw new Error('post-open observations disagree');
        name='omnigrex_request_review'; args={operation_id:'controlled-handoff',summary:'Validated explicit absence and post-open scoped reads.'};
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
