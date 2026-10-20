---
status: accepted
date: 2026-10-20
decision-makers: [Mariusz]
---
# Own ChatGPT subscription credentials in the controller

## Context

Omnigrex needs a supported ChatGPT subscription alternative to OpenAI API keys.
Pi supports direct ChatGPT authentication, but disposable Runtime Processes cannot independently own a shared rotating refresh token.
The operator selected one ChatGPT account per installation, unattended renewal, and support on Docker and Microsandbox.

## Decision

Select subscription or API-key authentication explicitly for the `openai` provider without changing Agent Profile model identifiers or falling back to paid API requests.
The controller owns browser authentication, verified identity, protected persistent credentials, serialized renewal, and renewal during idle periods.
Runtime Processes retrieve access-only tokens through a private, authenticated, execution-fenced interface before model requests and call OpenAI directly.
Use `/runtime/provider-auth` for this plain HTTP interface, sharing the private listener and Turn authority with `/mcp` while keeping separate protocol paths.
Keep credentials separate from PostgreSQL Workflow records and Retained Runtime State.

Successful ordinary login may replace the account while Agent Turns are active.
Subsequent credential acquisitions use the replacement account, including existing Agent Sessions and their conversational context.
Failed login preserves the working login, and stale refresh operations must not overwrite a replacement.

Confirmed subscription quota and reauthorization failures durably block subscription-backed admission and cause affected Workflows to enter Human Handoff.
Temporary failures use bounded retries.
Operator recovery clears the installation block without automatically resuming Work Items.
Login, status, logout, and quota-unblock commands are operator operations.

## Rationale

Copying full OAuth credentials into each guest loses rotated credentials and races concurrent renewal.
A shared writable guest credential file weakens ownership and depends on backend-specific sharing semantics.
A full inference proxy would protect access tokens too, but would add streaming and provider-protocol mediation to this feature.
Controller-owned refresh preserves direct inference while giving the durable login one owner.

## Consequences

This extends the deferred OAuth work in decisions 0007 and 0014; their API-key delivery and trusted-repository boundaries remain applicable.
Guests can read issued access tokens, and logout cannot guarantee revocation of tokens already issued.
Interrupted remote refresh rotation can require reauthorization because remote rotation and local persistence are not one atomic transaction.
Real subscription qualification on both backends is required before claiming supported operation.

## Links

- [Implementation plan](../plans/2026-10-20-chatgpt-subscription.md)
- [Disposable API-key delivery](0007-supply-provider-api-keys-through-disposable-files.md)
- [Owned Pi runner](0014-own-pi-resource-loading-with-a-pinned-runner.md)
- [OpenAI direct authentication](https://developers.openai.com/siwc/token-sharing-open-source/sign-in)
