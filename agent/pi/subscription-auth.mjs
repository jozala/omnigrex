import { AsyncLocalStorage } from 'node:async_hooks';
import { lazyStream } from '@earendil-works/pi-ai';
import { newDiagnostic, safeDiagnostic, normalizedCause, requestIDHash, providerCodes } from './provider-diagnostic.mjs';
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
function observeFailures(response, record, interrupted) {
  if (!response.body || !response.headers.get('content-type')?.includes('text/event-stream')) return response;
  const decoder = new TextDecoder();
  const parser = createParser({ onEvent(message) {
    try {
      const event = JSON.parse(message.data);
      const code = message.event === 'error' || event.error ? event.error?.code ?? event.code
        : event.type === 'response.failed' ? event.response?.error?.code
          : event.type === 'error' ? event.code : undefined;
      if (message.event === 'error' || event.error || ['response.failed', 'error'].includes(event.type)) record(code);
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
  const reader = response.body.getReader();
  const observed = new ReadableStream({
    async pull(controller) {
      try { const { done, value } = await reader.read(); if (done) controller.close(); else controller.enqueue(value); }
      catch (error) { interrupted(error); controller.error(error); }
    },
    cancel(reason) { return reader.cancel(reason); },
  });
  const body = observed.pipeThrough(new TransformStream({
    transform(chunk, controller) { feed(decoder.decode(chunk, { stream: true })); controller.enqueue(chunk); },
    flush() { feed(decoder.decode()); parser.reset({ consume: true }); },
  }));
  return new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers });
}

// Pi 1.0.4 emits flat function tools and may emit system-role messages for
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
  const calls = new AsyncLocalStorage();
  const evidence = new WeakMap();
  const state = () => calls.getStore();
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
    const call = state();
    if (call) call.diagnostic.stage = 'credential';
    try {
      const response = await request('GET', undefined, signal);
      if (call) call.diagnostic.http_status = response.status;
      const token = await response.json();
      if (!response.ok) {
        if (call) { call.failure = failures.has(token.failure) ? token.failure : 'subscription_temporary'; call.diagnostic.cause = 'http'; }
        throw new Error('Credential unavailable');
      }
      if (typeof token.access_token !== 'string' || !token.access_token || token.access_token.startsWith('sk-') || /[\0\r\n]/u.test(token.access_token) || typeof token.generation !== 'string' || !/^[0-9a-f-]{36}$/u.test(token.generation) || !Number.isFinite(Date.parse(token.expires_at)) || Date.parse(token.expires_at) <= Date.now()) throw new Error('Invalid credential response');
      if (call) { call.generation = token.generation; call.diagnostic.http_status = null; call.diagnostic.stage = 'local'; }
      return { auth: { apiKey: token.access_token }, source: 'ChatGPT subscription (controller-managed)' };
    } catch (error) {
      if (call) { call.failure ||= 'subscription_temporary'; if (call.diagnostic.cause !== 'http') call.diagnostic.cause = normalizedCause(error, signal); }
      throw new Error(`Omnigrex ${call?.failure ?? 'subscription_temporary'}`);
    }
  };
  const wrap = stream => (model, context, options = {}) => stream(model, context, {
    ...options, maxRetries: 0,
    onPayload: async (payload, selected) => {
      const call = state();
      if (call) call.diagnostic.stage = 'payload';
      try { const result = subscriptionPayload(await options.onPayload?.(payload, selected) ?? payload); if (call) call.diagnostic.stage = 'local'; return result; }
      catch { if (call) { call.failure = 'subscription_configuration'; call.diagnostic.cause = 'validation'; } throw new Error('Omnigrex subscription_configuration'); }
    },
    fetch: async (url, init) => {
      const call = state();
      let response;
      if (call) { call.diagnostic.stage = 'transport'; call.diagnostic.http_status = null; }
      try { response = await (options.fetch ?? fetcher)(url, { ...init, redirect: 'error' }); }
      catch (error) {
        if (call) call.diagnostic.cause = normalizedCause(error, init?.signal);
        throw error;
      }
      if (call) {
        call.diagnostic.http_status = response.status;
        call.diagnostic.request_id_hash = requestIDHash(response);
        call.diagnostic.stage = response.ok ? 'provider_stream' : 'provider_http';
        if (response.ok && !response.headers.get('content-type')?.includes('text/event-stream')) call.diagnostic.cause = 'protocol';
      }
      if (!response.ok) {
        const code = await httpFailureCode(response);
        if (call) {
          call.failure = structuredFailures.get(code) ?? (response.status === 401 ? 'subscription_auth'
            : [400, 403, 404].includes(response.status) ? 'subscription_configuration' : 'subscription_temporary');
          call.diagnostic.cause = 'http';
          call.diagnostic.code = providerCodes.has(code) ? code : 'unknown';
        }
        return response;
      }
      return observeFailures(response, code => {
        if (call) {
          call.failure = structuredFailures.get(code) ?? 'subscription_temporary';
          call.diagnostic.cause = 'stream_error';
          call.diagnostic.code = providerCodes.has(code) ? code : 'unknown';
        }
      }, error => { if (call) call.diagnostic.cause = normalizedCause(error, init?.signal); });
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
  // Own each complete request, including credential resolution before provider
  // streaming. Bind evidence to the actual terminal message, not "last error"
  // globals: concurrent calls and later Turns cannot inherit prior evidence.
  for (const method of ['stream', 'streamSimple']) {
    const original = runtime[method].bind(runtime);
    runtime[method] = (...args) => {
      const call = { diagnostic: newDiagnostic(), generation: '', failure: '', started: performance.now() };
      const output = calls.run(call, () => lazyStream(args[0], async () => {
        const input = original(...args);
        return {
          async *[Symbol.asyncIterator]() {
            for await (const event of input) {
              if (event.type === 'error') {
                call.diagnostic.elapsed_ms = Math.min(86400000, Math.round(performance.now() - call.started));
                if (event.error.stopReason === 'aborted') {
                  const cause = normalizedCause(undefined, args[2]?.signal);
                  if (cause !== 'unknown') call.diagnostic.cause = cause;
                  else if (call.diagnostic.cause !== 'timeout') call.diagnostic.cause = 'cancelled';
                }
                evidence.set(event.error, call);
              }
              yield event;
            }
          },
          result: () => input.result(),
        };
      }));
      return output;
    };
  }
  return { runtime,
    async failure(source) {
      const call = evidence.get(source);
      const failure = call?.failure || 'subscription_temporary';
      const generation = call?.generation || '';
      if (generation && ['subscription_quota', 'subscription_auth'].includes(failure)) {
        try { await request('POST', { generation, failure }); } catch { /* Worker retries from sanitized evidence. */ }
      }
      return { failure, generation, diagnostic: safeDiagnostic(call?.diagnostic) };
    },
  };
}
