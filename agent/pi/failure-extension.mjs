import { safeDiagnostic } from './provider-diagnostic.mjs';
import { isRetryableAssistantError } from '@earendil-works/pi-ai/compat';

const counter = value => Number.isFinite(value) ? value : 0;

// Pi invokes message_end extensions before notifying listeners and appending
// the finalized message to SessionManager. Reconstruct failure records there,
// rather than redacting files or Go events after secret-bearing text is saved.
export function failureExtension(pi, subscription) {
  pi.on('message_end', (event, ctx) => {
    const source = event.message;
    if (source?.role !== 'assistant' || !['error', 'aborted'].includes(source.stopReason)) return;
    const retryable = typeof source.errorMessage === 'string' && isRetryableAssistantError(source);
    const usage = source.usage;
    // A context invalidated during shutdown must not make this hook throw:
    // Pi catches extension exceptions and would otherwise persist the source.
    let model;
    try { model = ctx.model; } catch { /* Retain only unknown identity below. */ }
    const sanitized = { message: {
      role: 'assistant', content: [],
      // Identity comes from the selected runtime model, not provider responses.
      api: model?.api ?? 'unknown', provider: model?.provider ?? 'unknown', model: model?.id ?? 'unknown',
      usage: {
        input: counter(usage?.input), output: counter(usage?.output),
        cacheRead: counter(usage?.cacheRead), cacheWrite: counter(usage?.cacheWrite), totalTokens: counter(usage?.totalTokens),
        cost: {
          input: counter(usage?.cost?.input), output: counter(usage?.cost?.output),
          cacheRead: counter(usage?.cost?.cacheRead), cacheWrite: counter(usage?.cost?.cacheWrite), total: counter(usage?.cost?.total),
        },
      },
      stopReason: source.stopReason,
      // Preserve Pi's retryable/non-retryable distinction using fixed messages
      // recognized by its pinned classifier, without retaining source text,
      // partial content, response IDs, or arbitrary diagnostic metadata.
      errorMessage: source.stopReason === 'aborted' ? 'Provider request aborted'
        : retryable ? 'Provider returned error' : 'Provider request failed',
      timestamp: Number.isFinite(source.timestamp) ? source.timestamp : Date.now(),
    } };
    sanitized.message.omnigrexProviderDiagnostic = safeDiagnostic(undefined);
    if (!subscription) return sanitized;
    return Promise.resolve().then(() => subscription.failure(source)).then(({ failure, generation, diagnostic }) => {
      if (source.stopReason === 'error') sanitized.message.errorMessage = `Omnigrex ${['subscription_quota', 'subscription_auth', 'subscription_configuration', 'subscription_temporary'].includes(failure) ? failure : 'subscription_temporary'}`;
      sanitized.message.omnigrexProviderDiagnostic = safeDiagnostic(diagnostic);
      sanitized.message.omnigrexProviderGeneration = generation;
      return sanitized;
    }).catch(() => {
      // An extension exception would let Pi persist the unsanitized source.
      if (source.stopReason === 'error') sanitized.message.errorMessage = 'Omnigrex subscription_temporary';
      return sanitized;
    });
  });
}
