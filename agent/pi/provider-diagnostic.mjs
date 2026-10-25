import { randomUUID, createHash } from 'node:crypto';

// Only fixed vocabularies cross the persistence boundary. Never copy an SDK
// message, URL, response body, or arbitrary property into this schema.
export const providerCodes = new Set([
  'subscription_sharing_usage_limit_exceeded', 'subscription_sharing_invalid_user',
  'subscription_sharing_user_not_eligible', 'subscription_sharing_unsupported_capability',
  'subscription_sharing_route_not_supported', 'chatpass_v2_scope_not_authorized',
  'chatpass_v2_invalid_authorization_context', 'subscription_sharing_usage_unavailable',
  'subscription_sharing_user_unavailable', 'model_not_found',
]);
const stages = new Set(['credential', 'payload', 'provider_http', 'provider_stream', 'transport', 'local', 'unknown']);
const causes = new Set(['unknown', 'network', 'timeout', 'cancelled', 'protocol', 'validation', 'http', 'stream_error']);
export function newDiagnostic() {
  return { attempt: randomUUID(), stage: 'unknown', cause: 'unknown', retryability: 'unknown', http_status: null, code: 'unknown', request_id_hash: null, elapsed_ms: 0, auth_mode: 'subscription' };
}
export function safeDiagnostic(source) {
  // Getters/proxies are untrusted too. Extraction failure must stay sanitized.
  const result = { stage: 'unknown', cause: 'unknown', retryability: 'unknown', http_status: null, code: 'unknown', request_id_hash: null, elapsed_ms: null, auth_mode: 'unknown', attempt: null };
  try {
    if (!source || typeof source !== 'object') return result;
    if (stages.has(source.stage)) result.stage = source.stage;
    if (causes.has(source.cause)) result.cause = source.cause;
    if (['unknown', 'yes', 'no'].includes(source.retryability)) result.retryability = source.retryability;
    if (Number.isInteger(source.http_status) && source.http_status >= 100 && source.http_status <= 599) result.http_status = source.http_status;
    if (providerCodes.has(source.code)) result.code = source.code;
    if (typeof source.attempt === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u.test(source.attempt)) result.attempt = source.attempt;
    if (typeof source.request_id_hash === 'string' && /^[0-9a-f]{64}$/u.test(source.request_id_hash)) result.request_id_hash = source.request_id_hash;
    if (Number.isInteger(source.elapsed_ms) && source.elapsed_ms >= 0 && source.elapsed_ms <= 86400000) result.elapsed_ms = source.elapsed_ms;
    if (source.auth_mode === 'subscription') result.auth_mode = 'subscription';
  } catch { return { ...result, stage: 'unknown', cause: 'unknown' }; }
  return result;
}
export function normalizedCause(error, signal) {
  try {
    if (signal?.aborted) return signal.reason?.name === 'TimeoutError' ? 'timeout' : 'cancelled';
    if (error?.name === 'AbortError') return 'cancelled';
    if (['TimeoutError', 'APIConnectionTimeoutError'].includes(error?.name)) return 'timeout';
    const code = error?.cause?.code ?? error?.code;
    if (['ETIMEDOUT', 'UND_ERR_CONNECT_TIMEOUT', 'UND_ERR_HEADERS_TIMEOUT', 'UND_ERR_BODY_TIMEOUT'].includes(code)) return 'timeout';
    if (['ECONNRESET', 'ECONNREFUSED', 'ENOTFOUND', 'EAI_AGAIN', 'ENETUNREACH', 'UND_ERR_SOCKET'].includes(code)) return 'network';
  } catch { /* Unknown is evidence, not permission to copy the exception. */ }
  return 'unknown';
}
export function requestIDHash(response) {
  try {
    const id = response.headers.get('x-request-id');
    // Even a syntactically valid identifier could be a credential. Retain only
    // its digest, explicitly named as such, never an arbitrary header value.
    if (id && id.length <= 200 && /^[A-Za-z0-9_-]+$/u.test(id)) return createHash('sha256').update(id).digest('hex');
  } catch { /* Missing correlation is allowed. */ }
  return null;
}
