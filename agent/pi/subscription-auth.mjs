import { ModelRuntime } from '@earendil-works/pi-coding-agent';
import { createParser } from 'eventsource-parser';

const failures = new Set(['subscription_quota', 'subscription_auth', 'subscription_configuration', 'subscription_temporary']);
const structuredFailures = new Map([
  ['subscription_sharing_usage_limit_exceeded', 'subscription_quota'],
  ['subscription_sharing_invalid_user', 'subscription_auth'],
  ['subscription_sharing_user_not_eligible', 'subscription_configuration'],
  ['subscription_sharing_unsupported_capability', 'subscription_configuration'],
  ['subscription_sharing_route_not_supported', 'subscription_configuration'],
  ['chatpass_v2_scope_not_authorized', 'subscription_configuration'],
  ['chatpass_v2_invalid_authorization_context', 'subscription_configuration'],
  ['subscription_sharing_usage_unavailable', 'subscription_temporary'],
  ['subscription_sharing_user_unavailable', 'subscription_temporary'],
  ['model_not_found', 'subscription_configuration'],
]);

async function httpFailureCode(response) {
  try {
    const code = (await response.clone().json())?.error?.code;
    return typeof code === 'string' ? code : undefined;
  } catch { return undefined; }
}

// Observe only structured SSE error codes while forwarding the original bytes.
// Reuse the pinned SSE parser already used by MCP for CR, LF, and CRLF framing.
// Match the SDK's complete-event semantics: an unrelated large response output
// must not hide an error code. No raw event survives beyond this observation.
// Pi's parsed-event hook alone misses named `error` events: OpenAI's SDK throws
// those before yielding them to the provider adapter.
function observeFailures(response, record) {
  if (!response.body || !response.headers.get('content-type')?.includes('text/event-stream')) return response;
  const decoder = new TextDecoder();
  const parser = createParser({ onEvent(message) {
    try {
      const event = JSON.parse(message.data);
      const code = message.event === 'error' || event.error ? event.error?.code ?? event.code
        : event.type === 'response.failed' ? event.response?.error?.code
          : event.type === 'error' ? event.code : undefined;
      if (typeof code === 'string') record(code);
    } catch { /* The provider SDK owns stream validation and settlement. */ }
  } });
  let trailingCR = false;
  const feed = text => {
    if (!text) return;
    if (trailingCR && text.startsWith('\n')) text = text.slice(1);
    trailingCR = text.endsWith('\r');
    // A CR already terminates a line; do not defer classification until another
    // chunk or EOF. The SDK can stop reading immediately on a terminal event.
    // Skip its optional LF even when CRLF is split between chunks.
    parser.feed(text.replace(/\r\n?/gu, '\n'));
  };
  const body = response.body.pipeThrough(new TransformStream({
    transform(chunk, controller) { feed(decoder.decode(chunk, { stream: true })); controller.enqueue(chunk); },
    flush() { feed(decoder.decode()); parser.reset({ consume: true }); },
  }));
  return new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers });
}

// Pi emits flat function tools and may emit system-role messages for
// non-reasoning models. Normalize only this documented direct subscription path.
export function subscriptionPayload(source) {
  const payload = { ...source, store: false, stream: true };
  for (const field of ['background', 'conversation', 'max_output_tokens', 'max_tool_calls', 'metadata', 'moderation', 'multi_agent', 'prompt', 'prompt_cache_retention', 'prompt_cache_options', 'safety_identifier', 'temperature', 'top_logprobs', 'top_p', 'truncation', 'user', 'previous_response_id', 'service_tier']) delete payload[field];
  if (!Array.isArray(payload.input)) throw new Error('Omnigrex subscription_configuration');
  payload.input = payload.input.map(item => item.role === 'system' ? { ...item, role: 'developer' } : item);
  if (payload.tools?.length) {
    if (payload.tools.some(tool => !['function', 'custom'].includes(tool.type))) throw new Error('Omnigrex subscription_configuration');
    payload.input.push({ type: 'additional_tools', role: 'developer', tools: payload.tools });
  }
  delete payload.tools;
  return payload;
}

// No credential is stored in Pi. Native provider auth resolves each request
// through the controller, without ambient keys or local OAuth refresh fallback.
export async function subscriptionRuntime(configuration, agentDir, modelsPath, fetcher = fetch) {
  const endpoint = new URL(configuration.mcp.url);
  endpoint.pathname = '/runtime/provider-auth';
  endpoint.search = '';
  const headers = Object.fromEntries(configuration.mcp.headers.map(header => [header.name, header.value]));
  let generation = '';
  let observedFailure = '';
  // Private integration hint, not provider diagnostic text or a new error schema.
  let allowContextOverflow = false;
  const request = async (method, body, signal) => fetcher(endpoint.href, {
    method, headers: { ...headers, 'Content-Type': 'application/json', Accept: 'application/json' },
    body: body && JSON.stringify(body), redirect: 'error', signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(20000)]) : AbortSignal.timeout(20000),
  });
  const credentials = {
    async read() { return undefined; }, async list() { return []; },
    async modify() { throw new Error('Controller owns subscription credentials'); },
    async delete() { throw new Error('Controller owns subscription credentials'); },
  };
  const runtime = await ModelRuntime.create({ credentials, authPath: `${agentDir}/auth.json`, modelsPath, refreshOnCreate: false });
  const provider = runtime.getProvider('openai');
  if (!provider) throw new Error('Exact OpenAI provider unavailable');
  const resolve = async ({ signal }) => {
    observedFailure = '';
    allowContextOverflow = false;
    try {
      const response = await request('GET', undefined, signal);
      const token = await response.json();
      if (!response.ok) {
        observedFailure = failures.has(token.failure) ? token.failure : 'subscription_temporary';
        throw new Error('Credential unavailable');
      }
      if (typeof token.access_token !== 'string' || !token.access_token || token.access_token.startsWith('sk-') || /[\0\r\n]/u.test(token.access_token) || typeof token.generation !== 'string' || !/^[0-9a-f-]{36}$/u.test(token.generation) || !Number.isFinite(Date.parse(token.expires_at)) || Date.parse(token.expires_at) <= Date.now()) throw new Error('Invalid credential response');
      generation = token.generation;
      return { auth: { apiKey: token.access_token }, source: 'ChatGPT subscription (controller-managed)' };
    } catch {
      observedFailure ||= 'subscription_temporary';
      throw new Error(`Omnigrex ${observedFailure}`);
    }
  };
  const wrap = stream => (model, context, options = {}) => stream(model, context, {
    ...options, maxRetries: 0,
    onPayload: async (payload, selected) => {
      try { return subscriptionPayload(await options.onPayload?.(payload, selected) ?? payload); }
      catch { observedFailure = 'subscription_configuration'; throw new Error('Omnigrex subscription_configuration'); }
    },
    fetch: async (url, init) => {
      const response = await (options.fetch ?? fetcher)(url, { ...init, redirect: 'error' });
      if (!response.ok) {
        const code = await httpFailureCode(response);
        allowContextOverflow = [400, 413].includes(response.status) && !structuredFailures.has(code);
        observedFailure = structuredFailures.get(code) ?? (response.status === 401 ? 'subscription_auth'
          : [400, 403, 404].includes(response.status) ? 'subscription_configuration' : 'subscription_temporary');
        return response;
      }
      return observeFailures(response, code => {
        allowContextOverflow = ['context_length_exceeded', 'model_context_window_exceeded'].includes(code);
        observedFailure = structuredFailures.get(code) ?? 'subscription_temporary';
      });
    },
  });
  runtime.registerNativeProvider({ ...provider,
    auth: { apiKey: {
      name: 'ChatGPT subscription (controller-managed)',
      async check() { return { type: 'api_key', source: 'ChatGPT subscription (controller-managed)' }; },
      resolve,
    } },
    stream: wrap(provider.stream), streamSimple: wrap(provider.streamSimple),
  });
  await runtime.refresh({ allowNetwork: false, providers: ['openai'] });
  return { runtime,
    async failure() {
      const failure = observedFailure || 'subscription_temporary';
      if (generation && ['subscription_quota', 'subscription_auth'].includes(failure)) {
        // Complete this bounded report before persisting the sanitized failure.
        // The Go worker also receives the generation for a controller-local retry.
        try { await request('POST', { generation, failure }); } catch { /* Worker retries from sanitized evidence. */ }
      }
      return { failure, generation, allowContextOverflow };
    },
  };
}
