import { isRetryableAssistantError, isContextOverflow } from '@earendil-works/pi-ai/compat';

const counter = value => Number.isFinite(value) ? value : 0;

// Pi invokes message_end extensions before notifying listeners and appending
// the finalized message to SessionManager. Reconstruct failure records there,
// rather than redacting files or Go events after secret-bearing text is saved.
export function failureExtension(pi, subscription) {
  pi.on('message_end', (event, ctx) => {
    const source = event.message;
    if (source?.role !== 'assistant' || !['error', 'aborted'].includes(source.stopReason)) return;
    const retryable = typeof source.errorMessage === 'string' && isRetryableAssistantError(source);
    // Preserve only explicit context/input overflow. Pi also recognizes broad
    // phrases such as "token limit exceeded", which can describe account quota.
    // Never infer overflow from usage on a failed request or retain source text.
    const error = typeof source.errorMessage === 'string' ? source.errorMessage : '';
    const overflow = source.stopReason === 'error' && isContextOverflow(source) &&
      /context[_ ](?:length|window|size)|(?:prompt|input)(?: is)? too long|input token count.*exceeds the maximum/i.test(error) &&
      !/\b(?:401|403|429)\b|quota|rate.?limit|authentication|unauthorized|too many requests/i.test(error);
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
        : overflow ? 'Context length exceeded'
        : retryable ? 'Provider returned error' : 'Provider request failed',
      timestamp: Number.isFinite(source.timestamp) ? source.timestamp : Date.now(),
    } };
    if (!subscription || source.stopReason !== 'error') return sanitized;
    return subscription.failure(source).then(({ failure, generation, allowContextOverflow }) => {
      // Structured subscription quota/auth/configuration evidence wins over
      // provider prose, even when that prose looks like context overflow.
      if (overflow && allowContextOverflow) return sanitized;
      sanitized.message.errorMessage = `Omnigrex ${failure}`;
      sanitized.message.omnigrexProviderGeneration = generation;
      return sanitized;
    }).catch(() => {
      // An extension exception would let Pi persist the unsanitized source.
      sanitized.message.errorMessage = 'Omnigrex subscription_temporary';
      return sanitized;
    });
  });
}
