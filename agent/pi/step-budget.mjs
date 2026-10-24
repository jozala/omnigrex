// Enforcement owns the counter. Observation consumers must not count again.
export const summaryInstruction = 'Summarize completed work, unfinished work, and blockers. Tools are unavailable. An incomplete outcome is permitted. Do not present unfinished work as complete.';
const maxSteps = 2147483647;
export function stepBudget(limit, observe) {
  if (limit !== 'unlimited' && (!Number.isInteger(limit) || limit < 1 || limit > maxSteps)) throw new Error('Invalid step budget');
  let session, configuredTools;
  let active = false, used = 0, responses = 0, summary = false, stopped = false, settled = false, summaryText = '';
  const emit = complete => observe({ limit, used, responses, summary, stopped, complete }, summaryText);
  const extension = pi => {
    pi.registerCommand('omnigrex-step-budget-v1', { description: 'Per-Turn step budget enforced', handler: async () => {} });
    pi.on('context_with_system', event => {
      if (!active || !summary) return;
      // Unlike ordinary context hooks this runs AFTER forced-prompt projection.
      // Strip historical declarations too. These changes are request-only.
      return { messages: [...event.messages.map(message => {
        if (message.role !== 'system') return message;
        const { toolsAdded, toolsRemoved, ...rest } = message;
        return rest;
      }), { role: 'system', content: summaryInstruction, timestamp: Date.now() }] };
    });
    pi.on('tool_call', () => {
      // Also fences registered deferred/codemode calls via ctx.executeTool.
      if (active && summary) return { block: true, reason: 'Summary-only Agent Turn', terminate: true };
    });
    pi.on('agent_settled', () => {
      if (!active) return;
      // Pi clearQueue emits queue_update. Complete cleanup BEFORE RPC publishes
      // agent_settled, so the next admitted prompt cannot race a late event.
      if (summary) session.clearQueue();
      session.setActiveToolsByName(configuredTools);
      settled = true;
      emit(true);
    });
  };
  const attach = (current, tools) => {
    session = current; configuredTools = [...tools];
    const prompt = session.prompt.bind(session);
    session.prompt = async (...args) => {
      // Steering/follow-up input during this activity is not a new admission.
      if (active) return prompt(...args);
      used = responses = 0; summary = stopped = settled = false; summaryText = ''; active = true;
      session.setActiveToolsByName(configuredTools);
      emit(false);
      try { return await prompt(...args); }
      finally {
        if (!settled) {
          emit(false);
          if (summary) session.clearQueue();
          session.setActiveToolsByName(configuredTools);
        }
        active = false;
      }
    };
    const prepare = session.agent.prepareRequest;
    session.agent.prepareRequest = async (request, signal) => {
      if (!active) throw new Error('Model request without admitted prompt');
      // Normal enforcement is finishTurn + synchronous outer abort below.
      // This guard fails closed, and is never positive exhaustion evidence.
      if (!Number.isSafeInteger(used + 1) || limit !== 'unlimited' && used >= limit) throw new Error('Step budget boundary violated');
      used++;
      summary = limit !== 'unlimited' && used === limit;
      if (summary) session.setActiveToolsByName([]);
      emit(false);
      const result = await prepare?.(request, signal);
      return summary ? { ...result, context: { ...(result?.context ?? request.context), tools: [] } } : result;
    };
    const finish = session.agent.finishTurn;
    session.agent.finishTurn = async (turn, signal) => {
      responses++;
      const decision = await finish?.(turn, signal);
      if (summary) {
        const reason = turn.message.stopReason;
        stopped = reason === 'stop' || reason === 'toolUse';
        if (stopped) {
          const text = turn.message.content.filter(b => b.type === 'text').map(b => b.text).join('\n');
          // Never truncate or normalize before the controller redacts secrets:
          // either could leave an unrecognizable credential fragment. Omit an
          // oversized summary wholesale; the counter evidence is independent.
          summaryText = Buffer.byteLength(text, 'utf8') <= 2048 ? text : '';
        }
        emit(false);
        // finishTurn only ends the INNER loop. abort sets Pi's outer activity
        // flag synchronously, suppressing queues and pre-settle continuations.
        // Awaiting it in a running callback deadlocks on waitForIdle().
        void session.abort();
        return { action: 'end' };
      }
      emit(false);
      return decision;
    };
  };
  return { extension, attach };
}
