// Deterministic isolated provider scenario, not a real-model reasoning claim.
// All mutations still cross the production Pi MCP bridge and Store barriers.
const verified = 'REPLANNING_EXECUTABLE_MODEL_VERIFIED';

// Disposable executable product model: separate Node processes are actual
// restarts, and the test oracle is selected by the authorized commitment.
function modelSource(persistent) {
  return `import { existsSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
const persistent = ${persistent};
const file = process.env.CHECKLIST_STATE;
let state = persistent && existsSync(file) ? JSON.parse(readFileSync(file, 'utf8')) : { decisions: [] };
const action = process.argv[2];
if (action === 'set') {
  const input = JSON.parse(process.argv[3]);
  if (!['complete', 'defer'].includes(input.decision)) throw new Error('unsupported nonpersonal decision');
  state = { decisions: [{ decision: input.decision }] };
  if (persistent) writeFileSync(file, JSON.stringify(state));
} else if (action === 'reset') {
  rmSync(file, { force: true }); state = { decisions: [] };
} else if (action === 'template') {
  state = { label: 'Default checklist' };
} else if (action !== 'get') throw new Error('unsupported action');
console.log(JSON.stringify(state));
`;
}
const modelTest = `import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
const dir = mkdtempSync(join(tmpdir(), 'replanning-model-'));
const file = join(dir, 'decisions.json');
const persistent = process.argv[2] === 'persistent';
const decision = { decisions: [{ decision: 'complete' }] };
function run(...args) {
  const r = spawnSync(process.execPath, [resolve('checklist.mjs'), ...args], { env: { ...process.env, CHECKLIST_STATE: file }, encoding: 'utf8' });
  assert.equal(r.status, 0, r.stderr); return JSON.parse(r.stdout);
}
try {
  assert.deepEqual(run('set', JSON.stringify({ decision: 'complete', user_id: 'fixture-user-must-not-survive' })), decision);
  assert.deepEqual(run('get'), persistent ? decision : { decisions: [] }, 'restart must match the authorized persistence commitment');
  assert.equal(existsSync(file), persistent);
  if (persistent) assert.deepEqual(JSON.parse(readFileSync(file, 'utf8')), decision, 'persist only the nonpersonal decision');
  assert.deepEqual(run('template'), { label: 'Default checklist' });
  assert.deepEqual(run('reset'), { decisions: [] });
  assert.equal(existsSync(file), false, 'reset must delete the record');
  assert.deepEqual(run('get'), { decisions: [] });
  assert.deepEqual(run('template'), { label: 'Default checklist' }, 'a template label is not retained user state');
} finally { rmSync(dir, { recursive: true, force: true }); }
`;
function installModel(persistent) {
  return `cat > checklist.mjs <<'CHECKLIST_MODEL'
${modelSource(persistent)}CHECKLIST_MODEL
cat > checklist.test.mjs <<'CHECKLIST_TEST'
${modelTest}CHECKLIST_TEST
`;
}

export function replanningCall(envelope, results, unchanged) {
  const rawRead = i => { const c = results[i].content; return JSON.parse(typeof c === 'string' ? c : c.map(b => b.text ?? '').join('')); };
  // Supporting histories are deliberately omitted after implementation starts.
  // Read the frozen records through the real pinned bridge, not a transcript.
  const records = {};
  let consumed = 0;
  for (const collection of ['human_input', 'accepted_plan', 'planning_request', 'replanning', 'clarification_history']) {
    records[collection] = envelope[collection] ?? [];
    const count = envelope.collaboration_projection?.counts?.[collection] ?? 0;
    if (records[collection].length || !count) continue;
    for (let index = 0; index < count; index++) {
      let content = '', offset = 0;
      while (true) {
        if (consumed >= results.length) return {name:'omnigrex_get_collaboration_record', args:{collection,index,offset}};
        const page = rawRead(consumed++);
        if (page.index !== index || typeof page.content !== 'string') throw new Error('invalid frozen replanning retrieval');
        content += page.content;
        if (page.next_offset === undefined) break;
        offset = page.next_offset;
      }
      records[collection].push(JSON.parse(content));
    }
  }
  const read = i => rawRead(consumed + i);
  const role = envelope.triggering_event.role;
  const cycle = records.replanning?.[0]?.cycle;
  const input = records.human_input;
  const rounds = records.clarification_history;
  if (envelope.implementation_handoff) {
    const accepted = records.accepted_plan[0];
    if (!accepted?.published || accepted.id !== envelope.implementation_handoff.revision_id || !accepted.content?.requirements_brief || !accepted.content?.technical_plan || !accepted.original_request?.body) throw new Error('lost exact governing replanning pair');
  }
  const calls = [];
  const add = (name, args) => {
    const call = () => {
      const parameters = typeof args === 'function' ? args() : args;
      return { name, args: name === 'bash' ? { ...parameters, command: `set -eu\n${parameters.command}\nprintf '%s\\n' '${verified}'` } : parameters };
    };
    call.toolName = name; calls.push(call);
  };
  const op = suffix => `replanning-${suffix}-${envelope.agent_turn_id}`;
  const base = () => {
    const index = calls.length;
    add('omnigrex_refresh_default_branch', { operation_id: op('base') });
    return () => read(index).sha;
  };
  const assess = () => {
    if (input.length) add('omnigrex_assess_human_input', { operation_id: op('assess'), assessments: input.map(i => ({ input_id: i.id, evidence_ids: [i.id], disposition: ['implement', 'confirm-plan'].includes(i.kind) ? 'confirmed' : 'unchanged', explanation: 'The exact human choice is governing; no unasked changes are inferred.' })) });
  };
  const wait = () => add('omnigrex_request_clarification', { operation_id: op('wait'), questions: [] });
  const pair = (commit, revised) => ({ title: 'Restart-safe checklist', summary: revised ? 'Persist only nonpersonal checklist decisions.' : 'Transient checklist state.', status: 'proposed', changes: revised ? 'Supersede transient-only state: retain nonpersonal decisions across restart, with no identifiers.' : 'Initial complete pair: transient state.', requirements_brief: revised ? 'Checklist decisions survive restart. Never retain user identifiers. Reset deletes decisions. No accounts, external service or deployment.' : 'A transient local checklist; restart and reset discard state. No personal data, accounts, external service or deployment.', technical_plan: revised ? 'Use a local nonpersonal decision record and test restart plus reset. Helper names remain adjustable.' : 'Use transient local state; test restart discards it. Helper names remain adjustable.', unresolved_decisions: [], investigated_commit: commit(), evidence: ['README.md and the executable checklist.mjs restart/reset model'], decision_response_ids: input.filter(i => i.kind === 'answer').map(i => i.id) });
  if (role === 'PLANNER') {
    assess();
    const consent = input.find(i => i.kind === 'implement');
    if (cycle) {
      if (unchanged) {
        const commit = base();
        add('omnigrex_get_pull_request', {});
        add('omnigrex_complete_planning', () => ({ operation_id: op('resolve-unchanged'), revision_id: cycle.concern.accepted_revision_id, repository_commit: commit(), evidence: 'The fixture label represented a default checklist template, not retained user data. The original transient-state commitments remain valid.', material_decisions_resolved: true, disposition: 'unchanged' }));
      } else if (consent) {
        const commit = base();
        add('omnigrex_get_pull_request', {});
        add('omnigrex_complete_planning', () => ({ operation_id: op('resolve-revised'), revision_id: consent.target, repository_commit: commit(), evidence: 'The human authorized nonpersonal decisions to survive restart; reset and identifier exclusions remain mandatory. Current work head and base have been checked.', material_decisions_resolved: true, disposition: 'revised' }));
      } else if (input.some(i => i.kind === 'confirm-plan')) {
        wait(); // Confirmation is deliberately not resumption authority.
      } else if (input.some(i => i.kind === 'answer')) {
        const round = rounds.at(-1);
        add('omnigrex_resolve_clarification', { operation_id: op('resolve-human'), round_id: round.id, resolutions: [{ question_id: 'restart-policy', response_ids: input.map(i => i.id), explanation: 'Retain nonpersonal decisions across restart; never retain user identifiers.' }] });
        const commit = base();
        add('omnigrex_publish_plan_checkpoint', () => ({ operation_id: op('revised-pair'), content: pair(commit, true) }));
        wait();
      } else {
        add('omnigrex_request_clarification', { operation_id: op('material-question'), questions: [{ id: 'restart-policy', body: 'The isolated restart reproduction loses required decisions. Should nonpersonal checklist decisions survive restart while user identifiers remain excluded? I recommend that minimal persistence.' }] });
      }
    } else if (consent) {
      const commit = base();
      add('omnigrex_complete_planning', () => ({ operation_id: op('initial-ready'), revision_id: consent.target, repository_commit: commit(), evidence: 'The current minimal repository supports transient state; the isolated restart verification is explicit.', material_decisions_resolved: true }));
    } else if (input.some(i => i.kind === 'answer')) {
      const round = rounds.at(-1);
      add('omnigrex_resolve_clarification', { operation_id: op('initial-answer'), round_id: round.id, resolutions: [{ question_id: 'initial-policy', response_ids: input.map(i => i.id), explanation: 'Transient state with no personal-data retention.' }] });
      const commit = base();
      add('omnigrex_publish_plan_checkpoint', () => ({ operation_id: op('initial-pair'), content: pair(commit, false) }));
      wait();
    } else {
      add('omnigrex_request_clarification', { operation_id: op('initial-question'), questions: [{ id: 'initial-policy', body: 'Use transient checklist state with no personal-data retention?' }] });
    }
  } else if (role === 'DEVELOPER') {
    if (!cycle) {
      add('bash', { command: `${installModel(false)}node checklist.test.mjs transient
${unchanged ? '' : "if node checklist.test.mjs persistent; then echo 'missing failing restart control' >&2; exit 1; fi\n"}printf 'transient checklist\n' > feature.txt
git add feature.txt checklist.mjs checklist.test.mjs
git commit -m 'Preserve unfinished isolated restart model'` });
      add('omnigrex_publish_changes', { operation_id: op('partial-publish') });
      add('omnigrex_open_pr', { operation_id: op('partial-pr'), title: 'Restart-safe checklist', body: 'Unfinished useful checkpoint, not ready for review. Planner investigation needed.' });
      add('omnigrex_request_replanning', { operation_id: op('concern'), accepted_revision_id: envelope.implementation_handoff.revision_id, commitment: 'Transient state versus restart-safe checklist decisions.', evidence: unchanged ? Array.from({length:20},(_,i)=>`Isolated restart reproduction ${i}: ${'quoted '.repeat(800)}`) : ['The executable restart control (node checklist.test.mjs persistent) fails because transient state loses the decision; partial commits are preserved in this Change Proposal.'], consequences: 'The accepted transient-only assumption may invalidate the required behavior.', alternative: 'Persist nonpersonal decisions only, subject to human direction.' });
    } else {
      if (cycle.phase !== 'completed' || envelope.implementation_handoff.revision_id !== cycle.resolution.revision_id) throw new Error('resumption lost governing revision');
      if (!unchanged) {
        add('bash', { command: `${installModel(true)}node checklist.test.mjs persistent
printf 'nonpersonal persisted decisions; reset deletes decisions\n' > feature.txt
git add feature.txt checklist.mjs checklist.test.mjs
git commit -m 'Implement authorized restart-safe nonpersonal decisions'` });
        add('omnigrex_publish_changes', { operation_id: op('resumed-publish') });
      }
      add('omnigrex_get_pull_request', {});
      add('omnigrex_request_review', { operation_id: op('handoff'), summary: unchanged ? 'False alarm resolved; original transient commitments remain valid. Existing work is ready for review.' : 'Authorized nonpersonal restart-safe decisions implemented. Same Change Proposal and partial history retained; reset excludes identifiers.' });
    }
  } else if (role === 'REVIEWER') {
    if (!cycle || cycle.phase !== 'completed') throw new Error('review admitted without resolved cycle');
    add('omnigrex_get_pull_request', {});
    const preservedHead = cycle.scope.head_sha;
    if (!/^[0-9a-f]{40,64}$/.test(preservedHead)) throw new Error('missing immutable partial-work head');
    add('bash', { command: `git merge-base --is-ancestor '${preservedHead}' HEAD\nnode checklist.test.mjs ${unchanged ? 'transient' : 'persistent'}\n` + (unchanged ? "grep -q 'transient checklist' feature.txt" : "grep -q 'nonpersonal persisted decisions; reset deletes decisions' feature.txt") });
    add('omnigrex_submit_review', { operation_id: op('native-review'), event: 'APPROVE', body: 'Fresh evaluation of the exact accepted pair and resolved concern. The isolated restart model matches the governing human decision.', comments: [] });
  } else throw new Error('unexpected replanning Role');
  for (let i = 0; i < results.length - consumed; i++) {
    if (calls[i]?.toolName !== 'bash') continue;
    const result = results[consumed + i];
    const output = typeof result.content === 'string' ? result.content : result.content.map(b => b.text ?? '').join('');
    if (result.isError || output.trim().split(/\r?\n/).at(-1) !== verified) throw new Error('executable replanning verification failed; refusing subsequent publication or approval');
  }
  return calls[results.length - consumed]?.();
}
