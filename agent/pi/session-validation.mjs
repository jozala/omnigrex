// Validate the pinned v3 context contract before Pi's permissive loader can
// normalize malformed messages or ignore invalid context-affecting references.
const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const text = value => typeof value === 'string';
const name = value => text(value) && value.length > 0;
const number = value => typeof value === 'number' && Number.isFinite(value) && value >= 0;
const boolean = value => typeof value === 'boolean';
const optional = (value, valid) => value === undefined || valid(value);
const entryID = /^[A-Za-z0-9_-]{1,128}$/;
const thinkingLevels = new Set(['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']);
const stopReasons = new Set(['pending', 'stop', 'length', 'toolUse', 'error', 'aborted', 'deferred']);

function content(value, role) {
  if (text(value)) return ['user', 'system', 'custom'].includes(role);
  if (!Array.isArray(value)) return false;
  return value.every(block => {
    if (!object(block)) return false;
    switch (block.type) {
      case 'text': return text(block.text) && optional(block.textSignature, text);
      case 'image': return ['user', 'toolResult', 'custom'].includes(role) && text(block.data) && name(block.mimeType);
      case 'thinking': return role === 'assistant' && text(block.thinking) && optional(block.thinkingSignature, text) && optional(block.redacted, boolean);
      case 'toolCall': return role === 'assistant' && name(block.id) && name(block.name) && object(block.arguments);
      default: return false;
    }
  });
}

function usage(value) {
  return object(value) && ['input', 'output', 'cacheRead', 'cacheWrite', 'totalTokens'].every(key => number(value[key])) &&
    object(value.cost) && ['input', 'output', 'cacheRead', 'cacheWrite', 'total'].every(key => number(value.cost[key]));
}

function message(value) {
  if (!object(value) || !number(value.timestamp)) return false;
  if (value.role === 'bashExecution') return text(value.command) && text(value.output) && boolean(value.cancelled) && boolean(value.truncated) && optional(value.exitCode, Number.isInteger);
  if (!content(value.content, value.role)) return false;
  switch (value.role) {
    case 'user': return true;
    case 'system': return optional(value.sections, sections => object(sections) && Object.values(sections).every(section => section === null || text(section))) &&
      optional(value.toolsAdded, tools => Array.isArray(tools) && tools.every(tool => object(tool) && name(tool.name) && object(tool.parameters))) &&
      optional(value.toolsRemoved, tools => Array.isArray(tools) && tools.every(tool => object(tool) && name(tool.name)));
    case 'assistant': return name(value.api) && name(value.provider) && name(value.model) && usage(value.usage) && stopReasons.has(value.stopReason) && optional(value.errorMessage, text);
    case 'toolResult': return name(value.toolCallId) && name(value.toolName) && boolean(value.isError);
    case 'custom': return name(value.customType) && boolean(value.display);
    default: return false;
  }
}

function onBranch(id, parent, entries) {
  while (parent !== null) {
    if (parent === id) return true;
    parent = entries.get(parent).parentId;
  }
  return false;
}

function validEntry(entry, entries) {
  switch (entry.type) {
    case 'message': return message(entry.message);
    case 'model_change': return name(entry.provider) && name(entry.modelId);
    case 'thinking_level_change': return thinkingLevels.has(entry.thinkingLevel);
    case 'usage': return name(entry.kind) && name(entry.provider) && name(entry.model) && usage(entry.usage);
    case 'compaction': return text(entry.summary) && number(entry.tokensBefore) &&
      (entry.firstKeptEntryId === entry.id || onBranch(entry.firstKeptEntryId, entry.parentId, entries)) &&
      optional(entry.systemMessage, value => message(value) && value.role === 'system') && optional(entry.usage, usage) && optional(entry.fromHook, boolean);
    case 'branch_summary': return text(entry.summary) && (entry.fromId === 'root' || entries.has(entry.fromId)) && optional(entry.usage, usage) && optional(entry.fromHook, boolean);
    case 'custom': return name(entry.customType);
    case 'custom_message': return name(entry.customType) && content(entry.content, 'custom') && boolean(entry.display);
    case 'context_edit': {
      const target = entries.get(entry.targetId);
      if (!target || !onBranch(entry.targetId, entry.parentId, entries)) return false;
      const role = target.type === 'custom_message' ? 'custom' : target.type === 'message' ? target.message.role : '';
      return ['custom', 'user', 'assistant', 'toolResult'].includes(role) &&
        (entry.replacement === null || object(entry.replacement) && content(entry.replacement.content, role));
    }
    case 'label': return entries.has(entry.targetId) && optional(entry.label, text);
    case 'session_info': return optional(entry.name, text);
    default: return false;
  }
}

export function validateSessionEntries(lines) {
  const entries = new Map();
  for (const line of lines) {
    if (Buffer.byteLength(line) > 1 << 20 || entries.size >= 100000) throw new Error('Session entry limit exceeded');
    const entry = JSON.parse(line);
    if (!object(entry) || !text(entry.id) || !entryID.test(entry.id) || entries.has(entry.id) ||
        (entry.parentId !== null && !entries.has(entry.parentId)) || !text(entry.timestamp) || !Number.isFinite(Date.parse(entry.timestamp)) ||
        !validEntry(entry, entries)) throw new Error('invalid Session entry');
    entries.set(entry.id, entry);
  }
}
