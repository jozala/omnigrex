# Authenticate OpenAI with an API key or ChatGPT subscription

[Operator guide](README.md) · [Secret handling](secrets.md)

Start in the private deployment directory with the installation's exact `COMPOSE_FILE` exported.
Use matching controller and Pi images qualified for subscription inference and tool use on the selected backend.
The [implementation evidence](../plans/2026-10-20-chatgpt-subscription.md#evidence) distinguishes implemented behavior from completed live qualification.

## Choose the authentication mode

Set `OMNIGREX_OPENAI_AUTH_MODE=api_key` or `OMNIGREX_OPENAI_AUTH_MODE=subscription` in the deployment `.env`.
The default is `api_key` and preserves the existing [literal API-key bundle](secrets.md#create-secret-files).
Agent Profiles keep selecting `openai/model`; the selected model must exist in the pinned Pi catalog and be available to the signed-in account.
The direct subscription flow uses OpenAI's public Responses endpoint, not the legacy `openai-codex` provider.

Subscription mode uses one operator-managed ChatGPT account across the installation.
Concurrent Developer and Reviewer agents share its usage limits.
It never falls back to an OpenAI API key, even if the API-key bundle contains one.
Other providers continue using their selected API-key entries.
For subscription-only use, clear `OMNIGREX_PROVIDER_CREDENTIALS_FILE` in `.env`; Compose supplies an empty `/dev/null` mount without requiring a placeholder key.

Apply a mode change with the same Compose selection:

```sh
docker compose up -d --force-recreate orchestrator
```

Do not change an immutable Participant image to enable the feature; follow [Runtime Profile upgrades](upgrades.md) when a retained image lacks the subscription adapter.

## Sign in

Run:

```sh
docker compose exec orchestrator /usr/local/bin/omnigrex auth login
docker compose exec orchestrator /usr/local/bin/omnigrex auth status
```

Open the printed **Continue with ChatGPT** URL on your computer and authorize Omnigrex.
If the browser cannot reach the deployment's loopback callback, copy the full final redirect URL from the browser address bar and paste it into the waiting terminal.
Do not send that one-time URL through an Issue, log, or support transcript.
No Pi installation or provider key is required on your computer.

The controller verifies OAuth state, PKCE, the ID-token signature, issuer, audience, expiry, nonce, and direct-inference permission before activating the login.
Returning login reuses the saved registration.
To select another account or workspace, use ordinary login with the account-selection option:

```sh
docker compose exec orchestrator /usr/local/bin/omnigrex auth login --new-account
```

Successful login replaces the active account without waiting for Agent Turns to finish.
Subsequent model requests from existing Agent Sessions can send their conversational context to the replacement account.
Requests already in flight can finish using the previous account.
A failed login preserves the working credentials.

Commands coordinate through the running controller's owner-only Unix socket.
Before initial startup, an offline command can own the same directory exclusively, for example with `docker compose run --rm --no-deps orchestrator auth login`.
It cannot become a second writer while a controller owns that directory.

## Renewal and storage

The controller serializes refresh and saves replacement credentials atomically before issuing renewed access tokens.
It also renews while idle; being offline beyond OpenAI's renewable-grant lifetime can require another login.
An interrupted remote refresh whose replacement was not saved also requires reauthorization.
Network failures use bounded refresh backoff and do not delete the login.

Compose persists credentials in the controller-only `omnigrex-provider-auth` volume at `/var/lib/omnigrex/provider-auth`.
The directory is mode 0700 and the credential file is mode 0600, owned by UID/GID 10001.
Guests retrieve access-only tokens through the private `/runtime/provider-auth` HTTP endpoint before model requests.
It shares the listener and per-Turn authorization with `/mcp`, but is separate from the MCP protocol and agent-visible tools.
The refresh token, ID token, durable credential directory, and administrative socket are never mounted into guests.
Repository code can still read access tokens intentionally supplied to its Runtime Process.

Back up this directory separately as an encrypted deployment secret, with the controller and offline authentication commands stopped.
Do not include it in Session exports, ordinary runtime-state archives, or unencrypted recovery manifests.
Restore it only with the original writer fenced; an old backup can contain a superseded refresh token and require login again.
The host identifier is part of this secret-state directory and remains stable through ordinary restart and logout.

## Recover access

Inspect safe state:

```sh
docker compose exec orchestrator /usr/local/bin/omnigrex auth status
```

`reauthorization_required` means complete login again.
`quota_blocked` means review [ChatGPT Settings > Usage](https://chatgpt.com/settings/usage), including Omnigrex's app limit.
Omnigrex does not infer a reset time or assume that the entire subscription is exhausted.
After restoring capacity, explicitly permit another attempt:

```sh
docker compose exec orchestrator /usr/local/bin/omnigrex auth unblock
```

This clears only the quota block and does not prove that capacity is available.
A repeated limit blocks access again.
Login clears an authentication block but does not clear an independent quota block.
Both blocks survive controller recreation.

Confirmed quota and authentication failures prevent new subscription-backed execution and put affected Workflows into Human Handoff without consuming infrastructure retries.
Other providers remain usable.
Temporary provider/network failures use the ordinary bounded retry policy.
Unsupported models or request capabilities require correcting configuration rather than changing billing automatically.
After correcting the prerequisite, run repository doctor and [explicitly resume affected Work Items](troubleshooting.md#recover-a-blocked-workflow).
Authentication commands never resume Workflows automatically.

## Sign out

```sh
docker compose exec orchestrator /usr/local/bin/omnigrex auth logout
```

Logout removes local tokens, stops background renewal and new issuance, and attempts remote session revocation.
If revocation cannot be confirmed, the command reports that fact and you can disconnect Omnigrex in ChatGPT Settings.
Already-issued access tokens are not guaranteed to become invalid immediately.
Logout retains the non-secret registration mapping and host identifier for later login.
