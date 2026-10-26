// Pi's public Session subscription is the projection boundary. Only the
// RPC consumer sees this view; persistence, extensions and the agent keep the
// original events. Do not intercept stdout or replace Pi's RPC implementation.
export function controlEvent(event) {
  switch (event.type) {
    case 'agent_end':
      return { type: event.type, willRetry: event.willRetry };
    case 'message_start':
      // Rejected requests can start with an already failed, unsanitized message.
      // Only finalized message_end has crossed pre-persistence sanitization.
      return { type: event.type, message: { role: event.message.role } };
    case 'message_end': {
      const { role, stopReason, errorMessage, omnigrexProviderGeneration } = event.message;
      return { type: event.type, message: { role, stopReason, errorMessage, omnigrexProviderGeneration } };
    }
    case 'message_update': {
      const update = event.assistantMessageEvent;
      // Stock toJsonEvent needs the selected tool identity for toolcall_start.
      // It removes partial before serialization. Other deltas are activity only
      // for Omnigrex, not a second transcript or a source of outcome evidence.
      return { type: event.type, message: { role: event.message.role }, assistantMessageEvent: {
        type: update.type, contentIndex: update.contentIndex,
        ...(update.type === 'toolcall_start' ? { partial: update.partial } : {}),
      } };
    }
    case 'turn_end':
    case 'entry_appended':
    case 'queue_update':
    case 'bash_execution_update':
      return { type: event.type };
    case 'tool_execution_start':
    case 'tool_execution_update':
    case 'tool_execution_end':
      return { type: event.type, toolCallId: event.toolCallId, toolName: event.toolName,
        parentToolCallId: event.parentToolCallId, isError: event.isError };
    case 'compaction_start':
      return { type: event.type, reason: event.reason };
    case 'compaction_end':
      // Summary calls bypass conversational message_end sanitization. Neither
      // summary bodies nor arbitrary provider errors belong on the RPC wire.
      return { type: event.type, reason: event.reason, aborted: event.aborted,
        willRetry: event.willRetry,
        errorMessage: event.errorMessage ? 'Omnigrex compaction_failed' : undefined };
    default:
      // Required lifecycle/retry fields and unknown future events are never
      // silently discarded. The controller's existing 1 MiB bound fails closed.
      return event;
  }
}

// Bind every ordinary method to its real SDK receiver (including private state).
// Cache session views so rebinds follow runtime.session without modifying the
// Session itself. subscribe's unsubscribe handle and synchronous ordering pass
// through unchanged; the agent's separate backpressure subscription is intact.
export function controlRuntime(runtime) {
  const sessions = new WeakMap();
  return new Proxy(runtime, {
    get(target, property) {
      if (property === 'session') {
        const session = target.session;
        if (!sessions.has(session)) sessions.set(session, new Proxy(session, {
          get(owner, key) {
            if (key === 'subscribe') return listener => owner.subscribe(event => listener(controlEvent(event)));
            const value = Reflect.get(owner, key, owner);
            return typeof value === 'function' ? value.bind(owner) : value;
          },
        }));
        return sessions.get(session);
      }
      const value = Reflect.get(target, property, target);
      return typeof value === 'function' ? value.bind(target) : value;
    },
  });
}
