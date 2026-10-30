# CI-gated Reviewer admission

Delivery plan for [Work Item 121](https://github.com/jozala/omnigrex/issues/121).
The prerequisite shared CI observer and verified test-merge contract are merged in `3a3352d`.

1. Reproduce ungated Reviewer allocation at the PostgreSQL preparation boundary.
2. Add strict repository scheduling policy and pure completion/grace/deadline evaluation using the shared observer.
3. Persist a logical request before external discovery, fence observation and final allocation, reuse requests for infrastructure retries, and coalesce waiting synchronizations.
4. Carry bounded release evidence through the actual launch/prompt path without changing review authority.
5. Reconcile a separate waiting label and a stable editable progress comment at both presentation locations through durable visible effects.
6. Update decisions and authoritative configuration, operations, user, and testing guidance.
7. Run focused regressions and repository gates, publish committed history, inspect CI including native qualification, and request review only after required verification.

The gate releases on completion, not success, and timeout is an explicit incomplete-evidence admission rather than an infrastructure failure.
No running Reviewer is interrupted for same-head CI or base movement.
