import test from 'node:test';
import { replanningCall } from './replanning.mjs';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import assert from 'node:assert/strict';
for (const unchanged of [true,false]) {
 test(unchanged ? 'false alarm retains transient behavior and rejects failed checks' : 'material revision implements real restart/reset behavior and preserves partial history', () => {
 const dir=mkdtempSync(join(tmpdir(),'model-qualification-'));
 const bash=command=>execFileSync('bash',['-c',command],{cwd:dir,encoding:'utf8',stdio:['ignore','pipe','pipe']});
 try {
  bash("git init -q; git config user.name Fixture; git config user.email fixture@example.invalid");
  const accepted=id=>({id,published:true,content:{requirements_brief:'complete',technical_plan:'complete'},original_request:{body:'complete'}});
  let e={agent_turn_id:'test',triggering_event:{role:'DEVELOPER'},implementation_handoff:{revision_id:'old'},accepted_plan:[accepted('old')]};
  const first=replanningCall(e,[],unchanged);assert.equal(first.name,'bash');
  const output=bash(first.args.command);assert.match(output,/REPLANNING_EXECUTABLE_MODEL_VERIFIED\n$/);
  assert.throws(()=>replanningCall(e,[{content:'failed verification\n'}],unchanged),/refusing/);
  assert.equal(replanningCall(e,[{content:output}],unchanged).name,'omnigrex_publish_changes');
  const partial=bash('git rev-parse HEAD').trim();
  const revised=unchanged?'old':'new';
  e={...e,implementation_handoff:{revision_id:revised},accepted_plan:[accepted(revised)],replanning:[{cycle:{phase:'completed',scope:{head_sha:partial},resolution:{revision_id:revised}}}]};
  if(!unchanged){const resumed=replanningCall(e,[],false);assert.equal(resumed.name,'bash');assert.match(bash(resumed.args.command),/REPLANNING_EXECUTABLE_MODEL_VERIFIED\n$/);}
  e.triggering_event.role='REVIEWER';
  const review=replanningCall(e,[{content:'{}'}],unchanged);assert.equal(review.name,'bash');
  const proof=bash(review.args.command);
  assert.throws(()=>replanningCall(e,[{content:'{}'},{content:'failed review\n'}],unchanged),/refusing/);
  assert.equal(replanningCall(e,[{content:'{}'},{content:proof}],unchanged).name,'omnigrex_submit_review');
 }finally{rmSync(dir,{recursive:true,force:true});}
 });
}
