# Operator Guide

This guide describes the supported first-iteration deployment and recovery procedures for Omnigrex.
Read the trust model before installing credentials or granting Docker access.

## Supported Deployment

The supported deployment is one Docker Compose stack running the Omnigrex orchestrator, PostgreSQL, and disposable OpenCode Runtime Processes on one Docker Engine.
Docker Engine API 1.45 or newer and Docker Compose v2 are required.
The operator must provide public DNS and HTTPS ingress for GitHub webhooks.
Production hosts deploy without cloning the repository by using published `linux/amd64` images from `ghcr.io/jozala/omnigrex/` and the pinned `deploy/compose.yaml` and `deploy/.env.example` files for one commit.
A source checkout is only needed for development and for qualifying Runtime Profile upgrades elsewhere.

Only trusted repositories are supported.
The orchestrator has effective host-root authority through the Docker socket and can read all deployment secrets.
Runtime Processes do not receive the Docker socket or GitHub credentials, but they do receive the deployment-wide provider credential bundle and outbound network access.
A malicious repository, dependency, plugin, instruction file, or agent command can expose provider credentials.
The provider bundle is not isolated by Role.
Narrow provider privileges, configure spending limits, and rotate the complete bundle after any suspected repository compromise.

Omnigrex does not enforce required GitHub checks before `omnigrex:pr-ready`.
Use GitHub branch protection when checks must be mandatory.

## Tracing

The orchestrator exports OpenTelemetry traces over OTLP/HTTP with protobuf encoding when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is configured.
Tracing is inactive when neither endpoint is set, or when `OTEL_SDK_DISABLED=true`.
Both Compose bundles pass the tracing settings from the deployment `.env` to the orchestrator.

Configure an OTLP-compatible collector's HTTP endpoint in your private deployment `.env`:

```dotenv
OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example:4318
OTEL_SERVICE_NAME=omnigrex
```

If the collector requires authentication, set `OTEL_EXPORTER_OTLP_HEADERS` using OpenTelemetry's URL-encoded `key=value` format.
Keep authentication headers in private deployment configuration alongside the other secrets described in [Prerequisites](#prerequisites).
Recreate the orchestrator container after changing tracing environment settings.
The generic endpoint automatically gets `/v1/traces` appended; `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, if set, is a complete trace URL and takes precedence.
`OTEL_EXPORTER_OTLP_TRACES_HEADERS` can override the generic headers for traces.
The exporter always uses OTLP/HTTP with protobuf encoding, so use the collector's HTTP endpoint rather than its gRPC endpoint.
`OTEL_EXPORTER_OTLP_PROTOCOL` is not needed and does not select a different exporter in this implementation.
For a collector running on the Docker host, use an address reachable from the orchestrator container; `127.0.0.1` inside the container refers to the container itself.
Docker Desktop provides `host.docker.internal`; on Linux Docker Engine, that name requires a host-gateway mapping in a Compose override, or use a reachable host IP address.
The collector must listen on an interface reachable from the container and have a trace receiver connected to its export pipeline.

Traces identify the service as `omnigrex` by default and include its build version.
Use `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` to override the service name or attach environment attributes.
The default sampler is `parentbased_always_on`, which samples new root traces and respects upstream sampling decisions.
Set `OTEL_TRACES_SAMPLER=always_on` to sample every trace, or use `parentbased_traceidratio` with `OTEL_TRACES_SAMPLER_ARG=0.1` to sample 10% of root traces.

Instrumentation covers incoming webhook and MCP HTTP requests and outbound GitHub API requests, with W3C trace-context and baggage propagation.
Health and readiness requests are excluded from incoming tracing.
Docker clients for readiness probes and exact Runtime Profile image availability checks also suppress tracing, including probe cleanup.
Operational Docker clients retain tracing for Runtime Process operations.
Request bodies, tool arguments, and agent prompts are not added as trace attributes.
Claimed preparation, execution, webhook processing, mutation recovery, runtime cleanup, and outcome reconciliation create fixed-name operation spans.
Runtime launch and outcome reconciliation normally nest under execution, and GitHub/Docker client spans inherit the calling operation's context.
Deferred terminal corroboration creates a separate reconciliation attempt, including credentials and durable acknowledgement.
MCP tool operations cover admitted work through finalization, even after HTTP disconnection; a tool error can be recorded despite HTTP 200.
Expected blocked outcomes are distinct from infrastructure failures.
Asynchronous Workflow workers start independent traces; trace context is not persisted across durable queues or propagated across the Runtime Process MCP boundary.
Search structured `workflow_id`, `agent_participant_id`, `agent_session_id`, and `agent_turn_id` fields to connect independent traces.
JSON stdout records emitted with a valid operation context include `trace_id` and `span_id`, including unsampled contexts; a trace link may have no stored trace when sampling or delivery omitted it.
Collection should preserve JSON fields and keep domain and trace identifiers out of log stream labels.
There is no direct OpenTelemetry log export.
Long execution spans export on completion; operation-start logs and shorter child spans provide earlier visibility.
Published orchestrator builds identify their commit SHA, while unversioned local builds retain `dev`.
Completed spans are batched and flushed after services stop, within `OMNIGREX_SHUTDOWN_TIMEOUT`.
Compose allows one minute before forcibly stopping the orchestrator, configurable with `OMNIGREX_STOP_GRACE_PERIOD`.
Keep that grace period longer than service draining and Agent Turn cleanup plus a separate `OMNIGREX_SHUTDOWN_TIMEOUT` budget for exporting the final traces, increasing it when those timeouts are increased.
Exporter delivery failures are reported by the OpenTelemetry SDK without stopping Workflow processing.
Application metrics are separately opt-in as described below; direct log export is not enabled.

## Application Metrics

The orchestrator exports the bounded application instrument set over OTLP/HTTP with protobuf encoding.
There is no public metrics endpoint.
Both Compose bundles accept the same metrics settings, and both environment examples document them.

| Configuration | Traces | Application metrics |
| --- | --- | --- |
| No endpoints | Disabled | Disabled |
| Generic `OTEL_EXPORTER_OTLP_ENDPOINT` only | Enabled | Disabled |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` only | Enabled | Disabled |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` only | Disabled | Enabled |
| Generic endpoint with `OTEL_METRICS_EXPORTER=otlp` | Enabled | Enabled |
| Trace and metrics signal-specific endpoints | Enabled | Enabled |
| `OTEL_METRICS_EXPORTER=none` | Existing endpoint-based behavior | Disabled |
| `OTEL_SDK_DISABLED=true` | Disabled | Disabled |

`OTEL_METRICS_EXPORTER=otlp` requires a generic or metrics-specific endpoint; unsupported exporter selections fail startup.
The generic endpoint appends `/v1/metrics`; the metrics-specific endpoint is a complete URL and takes precedence.
`OTEL_EXPORTER_OTLP_METRICS_HEADERS` overrides generic OTLP headers for metrics, using the same URL-encoded format and private configuration rules as tracing.
Use HTTP(S) endpoints and header authentication, not credentials embedded in URLs.
The default `OTEL_METRIC_EXPORT_INTERVAL` is `60000` milliseconds; `OTEL_METRIC_EXPORT_TIMEOUT` defaults to `30000` milliseconds and `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT` to `10000` milliseconds in Compose.
Configured timing values must be positive integers in milliseconds.
A longer interval reduces samples, not the number of distinct series.

Traces and metrics share service/build/resource identity.
SDK views exclude unplanned library metrics and drop non-allowlisted application dimensions.
Durable gauges use one read-only observation with a five-second deadline, including connection acquisition, and a four-second server statement timeout.
An observation failure omits all durable gauges for that collection and increments `omnigrex.observation.failures`; it never substitutes zeros or cached state.
Traces and metrics shut down concurrently under a fresh bounded shutdown context after services stop; the observation registration and database remain open until that finishes.
Delivery failures are reported using safe categories without stopping Workflow processing.

See the [application observability contract and deployment handoff](./observability/application.md) for instrument names, queue eligibility, series budget, dashboard/alert requirements, and outstanding deployment verification.

## Prerequisites

Install the following software on the deployment host:

- Docker Engine with API 1.45 or newer.
- Docker Compose v2.
- `curl` for downloading the pinned deployment files.
- A reverse proxy or tunnel that terminates publicly trusted HTTPS.

Git and `mise` are not required on a no-clone deployment host.
Use a separate source checkout with Git and `mise` only when qualifying a Runtime Profile upgrade.

The orchestrator installs repository tools inside its own container before launching Agent Turns, including compiling Go tools declared in `mise.toml`.
Its Compose memory limit is 2 GiB; allocate enough Docker host memory for that limit plus PostgreSQL and the Runtime Processes.
Tool installation uses a per-assignment directory in the shared mise volume for scratch space rather than the orchestrator's 16 MiB `/tmp` tmpfs, so the volume also needs room for Go compilation.
The scratch directory is removed before the Runtime Process starts.

Create the local configuration in the deployment directory:

```sh
install -m 0700 -d secrets
```

Do not commit `.env`, `secrets/`, provider credentials, private keys, compatibility artifacts containing deployment metadata, or backups.
See [Published Images And Deployment Files](#published-images-and-deployment-files) and [Deploy](#deploy) for how the deployment directory is created without cloning the repository.

## Published Images And Deployment Files

Every push to `main` that passes the `check` job publishes two `linux/amd64` images for discovery:

- `ghcr.io/jozala/omnigrex/orchestrator:sha-<full-commit-SHA>` from `Dockerfile`.
- `ghcr.io/jozala/omnigrex/opencode:sha-<full-commit-SHA>` from `agent/opencode/Dockerfile`.

No `latest` tag is published, and Pull Request runs publish neither image.
After the first publication, a repository/package administrator must make both GHCR packages public and confirm that a host can pull them without `docker login`.
Do not add registry credentials to the deployment bundle.

Select a commit whose `main` CI run successfully published both images.
Open that run and read its `Published Omnigrex images` workflow summary, which records the commit SHA, both `sha-<commit>` tags, and both resolved `sha256` digests.

Download the deployment files pinned to that exact commit into one empty host directory:

```sh
COMMIT=<full-commit-SHA>
mkdir -p omnigrex-deploy && cd omnigrex-deploy
curl -fsSLO "https://raw.githubusercontent.com/jozala/omnigrex/${COMMIT}/deploy/compose.yaml"
curl -fsSLO "https://raw.githubusercontent.com/jozala/omnigrex/${COMMIT}/deploy/.env.example"
cp .env.example .env
install -m 0700 -d secrets
```

There is no GitHub Release for every merge; the commit-pinned raw URLs are the distribution mechanism.

Pull the two matching `sha-<commit>` images and resolve their registry digests:

```sh
docker pull --platform=linux/amd64 "ghcr.io/jozala/omnigrex/orchestrator:sha-${COMMIT}"
docker pull --platform=linux/amd64 "ghcr.io/jozala/omnigrex/opencode:sha-${COMMIT}"
docker image inspect "ghcr.io/jozala/omnigrex/orchestrator:sha-${COMMIT}" --format '{{index .RepoDigests 0}}'
docker image inspect "ghcr.io/jozala/omnigrex/opencode:sha-${COMMIT}" --format '{{index .RepoDigests 0}}'
```

The inspected `...@sha256:...` values must match the digests in the workflow summary.
Record them in `.env` as `OMNIGREX_ORCHESTRATOR_IMAGE` and `OMNIGREX_OPENCODE_ACP_V1_IMAGE`, keep `OMNIGREX_OPENCODE_ACP_V1_PLATFORM=linux/amd64`, and fill in the host-specific settings and file-backed secrets described below.
The deployment bundle uses one OpenCode digest for the `opencode-image` validation service, `OMNIGREX_AGENT_IMAGE_REFERENCE`, and `OMNIGREX_OPENCODE_ACP_V1_IMAGE` so they cannot diverge.

## GitHub Apps

Create two GitHub Apps under the account or organization that owns the managed repositories.
The App names are operator-selected, but the Apps must have different numeric IDs and different private keys.
User authorization, callback URLs, and OAuth features are not required.

### Developer App

Configure the Developer App with these exact repository permissions:

| Repository permission | Access |
| --- | --- |
| Metadata | Read-only |
| Checks | Read-only |
| Actions | Read-only |
| Contents | Read and write |
| Issues | Read and write |
| Pull requests | Read and write |
| Workflows | Read and write |

Subscribe to exactly these repository events:

- Issues.
- Pull request.
- Pull request review.
- Repository.

The Repository subscription delivers `repository.created` so newly created
repositories are provisioned. The Developer App also receives the default
`installation.created` and `installation_repositories.added` deliveries.

Enable the webhook and configure:

| Setting | Value |
| --- | --- |
| URL | `https://PUBLIC_HOST/webhooks/github` |
| Content type | `application/json` |
| Secret | The exact contents of `secrets/github-webhook-secret` |
| SSL verification | Enabled |

The URL must use public HTTPS and exactly the `/webhooks/github` path without credentials, a query string, or a fragment.

### Reviewer App

Configure the Reviewer App with these exact repository permissions:

| Repository permission | Access |
| --- | --- |
| Metadata | Read-only |
| Contents | Read-only |
| Pull requests | Read and write |

Select no webhook events and disable the webhook.
The Reviewer App must not reuse the Developer App identity or private key.
The Reviewer App permission contract is unchanged; CI diagnostics reads use the Developer App.
After upgrading to a release that includes scoped CI diagnostics, approve the added Actions read-only permission on the Developer App installation, then run preflight for each managed repository before agents receive the new capabilities.

### Agent Participant Signatures

GitHub attributes every comment to the GitHub App that published it, so
comments from different Agent Profiles sharing one App look identical.
Every new Agent Participant comment, Pull Request comment, native review
body (including otherwise bodyless approvals), and inline review comment
ends with a visible footer identifying its Agent Profile:

```text
_By Omnigrex: `profile-name` [Role Display Name]_
```

The profile name comes from the Agent Participant's validated Agent Turn
identity and the display name from the configured Role catalog. The hidden
idempotency marker follows the footer. Historical comments and Human
Handoff diagnostics are never re-signed.

### Keys And Installation

Generate and download one private key from each App's settings page.
Omnigrex accepts RSA private keys encoded as PKCS#1 or PKCS#8 PEM.
Store the files at:

```text
secrets/github-developer-private-key.pem
secrets/github-reviewer-private-key.pem
```

Set the numeric App IDs in `.env`:

```text
OMNIGREX_GITHUB_DEVELOPER_APP_ID=12345
OMNIGREX_GITHUB_REVIEWER_APP_ID=67890
```

Open each App's installation page, select the owning account or organization, and grant the App access to every repository Omnigrex will manage.
When using selected-repository installations, add each new repository to both installations before running preflight.
Omnigrex creates short-lived installation tokens, so no personal access token is required.

## Secrets

Generate independent database and webhook secrets:

```sh
openssl rand -base64 32 > secrets/database-password
openssl rand -hex 32 > secrets/github-webhook-secret
```

Copy the two GitHub App private keys into the paths described above.
Copy one OpenCode authentication bundle for the deployment:

```sh
cp /secure/path/opencode-auth.json secrets/provider-credentials.json
chmod 0640 secrets/*
```

The provider credential file must contain the nonempty JSON object accepted by OpenCode's `OPENCODE_AUTH_CONTENT` setting.
For example, an API-key provider commonly has this shape:

```json
{
  "provider-name": {
    "type": "api",
    "key": "REPLACE_WITH_SECRET"
  }
}
```

The provider and model selected in the Agent Profiles must be present in the pinned OpenCode catalog and usable with these credentials.
The preflight validates file shape but deliberately does not make a paid provider request.

### Secret File Group

On Linux, read the Docker socket group ID:

```sh
stat -c '%g' /var/run/docker.sock
```

Set that value as `OMNIGREX_DOCKER_GID` in `.env`.
Use `OMNIGREX_DOCKER_GID=0` with Docker Desktop.

Create a dedicated host group for Omnigrex secret files using the host operating system's account-management tools.
Change `secrets/*` to that group and set its numeric group ID as `OMNIGREX_SECRET_GID`.
The secret group ID must differ from the Docker socket group ID, and unrelated host users must not belong to the secret group.

Docker socket group membership is equivalent to host-root authority.
Mounting the socket read-only prevents file writes through the mount but does not restrict Docker API operations.

## Agent Profiles

Every managed repository must contain one Agent Profile for every Workflow Role on its default branch before the first trigger.
Profiles are discovered as direct lowercase `*.md` children of:

```text
.omnigrex/team/
```

Filenames are arbitrary and do not define Profile identity or Role.
The current selector requires exactly one discovered Profile for each Workflow Role.
Additional Profiles for the same Role are rejected until multiple-Profile selection is configured.

Each file starts with strict YAML front matter followed by Role instructions.
The supported fields are:

| Field | Requirement |
| --- | --- |
| `name` | Repository-local stable identity using lowercase letters, digits, hyphens, or underscores |
| `role` | Role ID referenced by the active Workflow Definition, such as `DEVELOPER` or `REVIEWER` |
| `runtime` | Runtime Profile in `name/version` form, currently `opencode-acp/v1` |
| `model` | Provider model in `provider/model` form |
| `variant` | Optional provider-specific variant without whitespace |
| `steps` | Integer from 1 through 1000 |
| `permissions` | Nonempty map of local OpenCode tools to `allow` or `deny` |

Supported local permission names are `read`, `edit`, `glob`, `grep`, `list`, `patch`, `bash`, `task`, `webfetch`, `websearch`, `codesearch`, `todoread`, `todowrite`, `question`, and `skill`.
Every omitted permission defaults to deny.
The Reviewer cannot allow `edit` or `patch`.
Runtime-provided Omnigrex MCP tools are authorized separately from these local permissions.

### Instruction Ownership and Operator Guidance

Omnigrex supplies platform instructions and the current Workflow Stage's objective, completion criteria, and purpose-specific guidance.
Repository Agent Profiles select skills and describe personality, communication style, and repository preferences.
Skills provide engineering procedures rather than owning platform permissions or Workflow transitions.

Instruction precedence is platform and Stage contracts, operator Role guidance, operator common guidance, and repository preferences, in that order.
This describes how guidance is interpreted; actual capabilities and transitions remain code-enforced.
Instructions from the running deployment supersede older deployment guidance in retained Agent Session history.

Optionally set `OMNIGREX_AGENT_INSTRUCTIONS_DIR` to a clean absolute path inside the orchestrator containing:

```text
agent-instructions/
  common.md
  roles/
    DEVELOPER.md
    REVIEWER.md
```

Use exact Role IDs from the deployment's Role Policy Catalog, not repository Profile names.
The common file and individual Role files are optional; an unset directory adds no operator guidance.
A configured missing or unreadable directory, unexpected entry, unknown Role, non-text file, invalid UTF-8, NUL byte, or file exceeding 64 KiB fails startup and the operator-instruction preflight check.
The common and applicable Role instructions must also fit a combined 64 KiB JSON-encoded budget, including escaped characters.
Preflight and launch validate the fully composed OpenCode configuration against a 120 KiB limit so its environment entry stays below Linux's per-string execution limit.
Empty instruction files add no guidance.

Create a Compose override alongside the deployment files, for example `compose.instructions.yaml`:

```yaml
services:
  orchestrator:
    environment:
      OMNIGREX_AGENT_INSTRUCTIONS_DIR: /etc/omnigrex/instructions
    volumes:
      - type: bind
        source: ./agent-instructions
        target: /etc/omnigrex/instructions
        read_only: true
        bind:
          create_host_path: false
```

Create the host directory and make its files readable by the orchestrator before starting with both Compose files.
The directory is mounted only into the orchestrator; Runtime Processes receive composed text, not the files or host paths.
The standard Compose bundles also forward `OMNIGREX_AGENT_INSTRUCTIONS_DIR` if set through the deployment environment, but the operator must provide the corresponding mount.

Files are loaded once at orchestrator startup.
After editing them, recreate the orchestrator with the same Compose file set, for example:

```sh
docker compose -f compose.yaml -f compose.instructions.yaml up -d --force-recreate orchestrator
```

New launches, including Session Continuation and recovery, use the running binary's core/Stage guidance and its startup-loaded operator instructions.
There is no persisted composed-instruction snapshot to replay after an upgrade and no hot reload inside an executing Agent Turn.
Repository Profile provenance remains separate and follows the default-branch rules below.
See [decision 0002](./decisions/0002-use-current-deployment-agent-instructions.md) and [decision 0003](./decisions/0003-separate-personality-from-platform-and-workflow-guidance.md).

### Repository Skills

Both Roles can use native OpenCode skills when their Agent Profile includes `skill: allow` under `permissions`.
Omnigrex then registers `/workspace/.agents/skills` as an explicit skill source, including for the Reviewer with project configuration and external automatic skill discovery disabled.
This adds a repository skill source without enabling sub-agents or changing MCP authority.
Profiles without skill permission do not receive this explicit source.

Store each skill in `.agents/skills/<name>/SKILL.md` with front matter such as:

```yaml
---
name: repository-example
description: Explain the procedure and when the agent should load it.
---
```

Use a unique name matching the directory and put the procedure after the front matter.
The agent sees the skill description and can load its body on demand with the skill tool.
Put optional examples and references in the same skill directory, with explicit instructions about when to read them.
Keep personality, communication preferences, and skill selection in the Agent Profile; platform and Stage instructions supply operational rules and approval criteria.

Skills come from the prepared working checkout, not the default-branch snapshot used for Agent Profiles.
The Developer may initially use default-branch skills and later use Change Proposal skills; the Reviewer uses skills from its Change Proposal checkout.
New Runtime Processes rediscover skills when continuing retained Agent Sessions, but in-process edits are not guaranteed to hot-reload.
Omnigrex's repository Profiles require loading their Role skill on each applicable Agent Turn, and platform instructions require reporting a blocker when required guidance is unavailable.
Publish those Profiles and their required skills together so the next Turn can find them.
For an existing Change Proposal, ensure its checkout contains any newly required skills before starting or resuming work.

A Change Proposal can modify skill instructions used in its own review.
The separate [human approval gate for skill changes](https://github.com/jozala/omnigrex/issues/43) is planned, not currently enforced.
See [ADR 0012](./adr/0012-load-repository-skills-from-the-current-checkout.md) for the checkout-local decision and the [runtime compatibility notes](../agent/opencode/README.md#reviewer-isolation) for other Reviewer limitations.

### Git Identity and Review Handoffs

Workspace preparation sets repository-local `user.name=Omnigrex` and `user.email=agent@omnigrex.invalid` before the agent starts.
This fixed identity supports normal and merge commits without agent-side setup and does not change existing commits or global Git configuration.

Each new `request_review` call publishes its summary as a signed PR comment identifying the exact head before its terminal handoff can succeed.
This applies to the initial PR and subsequent requested-changes Turns; agents should not post a duplicate summary or call `open_pr` for an existing PR.
The next Stage waits for confirmed publication.
An uncertain response is reconciled against the exact reserved marker, summary, and signature instead of blindly posting again.
Existing bounded mutation recovery applies, with Human Handoff when uncertainty cannot be resolved.

Both Roles can use `get_handoff` to retrieve the latest successful summary for their scoped Workflow, PR, and head.
The response contains the durable handoff ID, head, summary, creation time, and `publication_confirmed`; it is `null` when no handoff matches.
Historical internal-only handoffs remain readable with `publication_confirmed=false`; they are not retroactively described as published comments.
Already completed historical intents retain their recovery compatibility, while new requests use the publication barrier.
The Reviewer treats the summary as a claim to verify against code and test evidence.
Native inline findings are submitted through `submit_review.comments`, with the review body reserved for findings without valid diff locations.

### Agent Turn Tool Paths

An optional `.omnigrex/turn-configuration.yaml` on the default branch requests disk-backed directories for tool environment variables.
Omnigrex reads it from the same pinned default-branch commit as the Agent Profiles for each Agent Turn and applies it to both Roles.
The file is strictly validated; unknown fields, unsupported sizes, unapproved variables, and invalid directories cause a configuration Human Handoff before the Runtime Process starts.
For example:

```yaml
version: 1
environment-paths:
  directories:
    build-tmp:
      lifecycle: turn
    go-build-cache:
      lifecycle: assignment
  environment:
    - name: TMPDIR
      directory: build-tmp
    - name: GOTMPDIR
      directory: build-tmp
    - name: GOCACHE
      directory: go-build-cache
```

Set `OMNIGREX_AGENT_PATH_ENV_ALLOWLIST` to a comma-separated list of approved variable names in the deployment configuration, such as `TMPDIR,GOTMPDIR,GOCACHE,GOPATH`.
The operator allowlist is shared across Roles; the repository may request only approved names and cannot supply path values.
Omnigrex always rejects runtime-control and credential-related names such as `OPENCODE_*`, `OMNIGREX_*`, `HOME`, and `PATH` even if listed by the operator.
Without the optional file, `TMPDIR` continues to use the 64 MiB `/tmp/opencode` tmpfs.
Requested paths are created on the disk-backed mise volume under an isolated Assignment subpath, outside the repository workspace and Change Proposal tree.
Turn-lifecycle scratch is removed after the Runtime Process stops; assignment-lifecycle caches remain across turns and are removed during Assignment collection.
These directories have no per-Assignment disk quota, like the existing workspace volume, so monitor available Docker-volume storage.
A background monitor measures only the Agent Participant assignment-lifecycle cache area (on disk under the historical `assignment-<ID>` prefix) every `OMNIGREX_ASSIGNMENT_TOOL_CACHE_POLL_INTERVAL` (default `5m`) and logs a soft warning when allocated disk usage reaches `OMNIGREX_ASSIGNMENT_TOOL_CACHE_WARNING_MIB` (default `1024` MiB).
The warning carries only `agent_participant_id`, `size_bytes`, and `threshold_bytes`, is emitted once per unchanged size, never blocks an Agent Turn, and never changes retention or cleanup behavior.
Measurement is read-only, counts allocated filesystem blocks including directories, and resumes retained directory streams across bounded scan slices without replaying entries or changing modes.
The monitor retains at most four active scans, each with at most 32 directory handles and 8192 hardlink identities; ordinary files require no retained identity set.
Collection, cache replacement, fatal errors, completion, and shutdown release retained handles.
Unreadable or unsafe entries and depth or hardlink limits produce partial (truncated) measurements, which never clear a warning based on a below-threshold partial sum.
Moving build output to disk does not increase the Agent Turn container's memory limit or constrain compiler parallelism.
This implementation changes the `opencode-acp/v1` Runtime Profile content hash; existing Agent Sessions bound to the earlier contract cannot continue unless the deployment resets their stored associations before upgrading.

Agent Profiles are loaded from the latest default-branch commit before each Agent Turn.
Directory listing and every Profile file are read from the same exact commit.
Every direct Markdown file is treated as configuration; malformed files and unknown or unreferenced Roles fail discovery.
A file can move without changing Profile identity when its front matter name remains unchanged.
A feature branch cannot replace the Reviewer Profile used to review that branch.
Changing the Runtime Profile reference of an existing Agent Participant does not migrate its Agent Session and creates a configuration Human Handoff.

Never place provider credentials, GitHub credentials, deployment secrets, or private endpoint credentials in an Agent Profile.

## Runtime Profile Image

A no-clone deployment uses the published OpenCode digest for the selected commit.
Set the digest from the CI workflow summary and matching platform in `.env`:

```text
OMNIGREX_OPENCODE_ACP_V1_IMAGE=ghcr.io/jozala/omnigrex/opencode@sha256:...
OMNIGREX_OPENCODE_ACP_V1_PLATFORM=linux/amd64
```

Only `linux/amd64` is published initially.
The exact digest must already be available to the deployment host's Docker Engine because Omnigrex does not pull Runtime Profile images automatically.
Pull it explicitly before startup (see [Deploy](#deploy)).
Keep every digest referenced by active or retained Agent Sessions available until the corresponding state is garbage-collected.
The same requirement applies when restoring a backup on another host: restore `.env`, secrets, compatibility artifacts, the deployed commit, and every referenced image digest before changing current data.

### Agent Turn Memory

Set `OMNIGREX_AGENT_TURN_MEMORY_MIB` in `.env` to a positive integer number of MiB per Agent Turn container.
The default is `512`; set `OMNIGREX_AGENT_TURN_MEMORY_MIB=1024` for 1 GiB (1073741824 bytes).
After changing `.env`, recreate the orchestrator with `docker compose up -d --force-recreate orchestrator` and run `doctor` for each repository.
Newly created Agent Turn containers and the doctor ACP probe use the new limit; running containers are not resized.
Changing this setting does not change the Runtime Profile binding or require a database reset.
At the default concurrency of two, 1 GiB per turn allows up to 2 GiB across Agent Turn containers in addition to other host processes.
The first deployment of the memory-independent `opencode-acp/v1` contract changes its existing content hash once.
Coordinate the agreed one-time database reset with active turns before that upgrade; it discards Omnigrex Workflow and session associations, including those for existing Issues and Pull Requests, while GitHub Issues, Pull Requests, and labels remain.
Later memory-setting changes do not require another reset.

## HTTPS Ingress

The Compose stack publishes its HTTP server on `${OMNIGREX_HTTP_PORT:-8080}` and does not publish the private MCP listener.
When the reverse proxy runs on the same host, bind the backend to loopback by setting:

```text
OMNIGREX_HTTP_PORT=127.0.0.1:8080
```

The reverse proxy must:

- Terminate TLS with a publicly trusted certificate.
- Forward `POST /webhooks/github` to `http://127.0.0.1:8080/webhooks/github` without changing the path.
- Preserve the exact raw request body.
- Preserve `X-Hub-Signature-256`, `X-GitHub-Delivery`, and `X-GitHub-Event`.
- Allow request bodies up to 25 MiB.
- Complete request headers within five seconds and the request within fifteen seconds.
- Keep port 8081 and the `omnigrex-agent` network private.

A minimal host-level Caddy configuration is:

```caddyfile
omnigrex.example.com {
  handle /webhooks/github {
    reverse_proxy 127.0.0.1:8080
  }

  respond 404
}
```

Restrict direct access to the Compose HTTP port with a host firewall when it cannot be bound to loopback.
Omnigrex does not consume forwarded-client headers and does not require the proxy to supply them.

After deployment, use the Developer App's Advanced page to inspect a recent delivery or request a redelivery.
A valid delivery must receive HTTP `202 Accepted`.
This GitHub-side delivery check is required because `doctor` cannot prove that the remote webhook secret equals the local secret or that public DNS, TLS, firewall, and proxy routing work end to end.

## Deploy

From the deployment directory containing only the pinned `compose.yaml`, `.env`, and operator-provided `secrets/`, pull the exact OpenCode digest into the Docker Engine before startup, validate Compose, and start the stack without `--build`.
Docker Compose loads `.env` automatically, but the shell does not, so source it first for the explicit pre-pull:

```sh
set -a
. ./.env
set +a
docker pull --platform=linux/amd64 "$OMNIGREX_OPENCODE_ACP_V1_IMAGE"
docker compose config --quiet
docker compose up --wait
```

Verify local health and dependency readiness:

```sh
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

Run preflight once for every managed repository:

```sh
docker compose exec orchestrator \
  /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

Every line must report `PASS`, and the command must exit with status 0.
Configuration failures are reported together before external checks begin.
Independent external checks continue after failures so the operator receives one diagnostic set.

The PostgreSQL check uses a read-only connection and does not apply migrations.
Docker resource inspection does not write named volumes.
The ACP check creates and removes one temporary Runtime Process with tmpfs-backed writable paths.
GitHub installation checks create short-lived tokens but do not modify repository content or start a Workflow.

Finally, verify a real GitHub webhook delivery from the Developer App.
Do not add `omnigrex:run` until preflight and webhook delivery verification both pass.

## Automatic Label Provisioning

When the Developer App gains access to a repository, Omnigrex queues provisioning of the five managed labels using only Developer App credentials:
Provisioning is asynchronous; wait until the managed labels, including `omnigrex:run`, appear before adding the trigger label to an Issue.

- A new Developer App installation provisions every accessible repository.
  Accessible repositories are enumerated through a paginated
  installation-token request, not the webhook repository list.
  Enumeration is bounded at 100 pages of 100 repositories; an installation
  with more than 10,000 accessible repositories fails observably for
  operator follow-up instead of provisioning partially.
- Explicitly added repositories are provisioned from the
  `installation_repositories.added` delivery.
- Repositories created under all-repository access are provisioned from the
  `repository.created` delivery.

Onboarding deliveries use a separate processor, so installation enumeration does not block Issue and Pull Request webhook processing.
Provisioning creates missing label names only, preserves the color and
description of existing labels, leaves unrelated labels untouched, and never
applies a state label to an Issue or Pull Request or starts a Workflow.
Duplicate or overlapping deliveries are harmless, one repository's failure
does not block other repositories, and terminal failures are recorded as
observable provisioning and delivery failures for operator follow-up.

Inspect webhook deliveries and their repository jobs together when provisioning fails.
A delivery in state `FAILED` with a `last_error` may have queued no jobs because its payload or installation enumeration failed, but valid entries in a mixed delivery or earlier committed batches can leave jobs even when a later attempt exhausts the delivery.
Look for `kind = 'PROVISION_MANAGED_LABELS'` rows in `jobs` for that delivery; each job's `idempotency_key` has the form `label-provisioning:<delivery-id>:<repository-id>` and its `payload` identifies the repository.
A delivery with only valid entries becomes `PROCESSED` after its jobs are queued; a later terminal failure of one repository appears on its `jobs` row with status `FAILED` and a `last_error`.
Provisioning jobs leave `normalized_event_id` empty because onboarding deliveries do not create normalized Workflow events.

Repositories installed before this provisioning was deployed are not backfilled on startup.
Their missing labels are created by the existing Workflow label reconciliation the next time a Workflow runs.
Labels deleted from an idle repository are likewise restored only when a Workflow runs.

Onboarding scale and webhook admission are bounded.
The ingress accepts webhook bodies up to 25 MiB, covering GitHub's 25 MB payload cap; larger bodies receive HTTP `413` before authentication or durable recording.
GitHub does not deliver payloads exceeding its own cap, so no webhook-triggered provisioning occurs in that case either.
Large installations within these limits are queued in bounded batches with claim renewal between batches; progress is observable as `PROVISION_MANAGED_LABELS` job rows.
Transient failures while converting a provisioning webhook into durable work defer the next delivery attempt with increasing delays and GitHub rate-limit timing; these deliveries allow up to eight attempts.
If onboarding was not delivered or an installation exceeds the 100-page enumeration limit, the operator must create the missing `omnigrex:run` repository label before it can be applied to an Issue.
The same manual trigger-label step applies to repositories installed before this feature when that label is absent.
After the first Workflow starts, existing Workflow label reconciliation creates any other missing managed labels.

## Start A Workflow

Create a small Issue with explicit acceptance criteria and add the `omnigrex:run` label.
Omnigrex creates missing workflow labels when needed.
Adding `omnigrex:run` to the Workflow's current Change Proposal invokes the same activation rules as adding it to the Work Item Issue.
The command requires the durable current Change Proposal association for the same repository, Pull Request ID, and Pull Request number.
Historical or replaced Pull Requests, unrelated Pull Requests, and a Workflow marker alone cannot activate a Workflow, and the command never creates a Work Item, adopts an external Pull Request, or reopens a closed Issue.

| Label | Meaning |
| --- | --- |
| `omnigrex:run` | Human command to create or reactivate a Workflow Attempt |
| `omnigrex:developing` | Developer work is active or pending |
| `omnigrex:reviewing` | Reviewer work is active or pending |
| `omnigrex:pr-ready` | The current Pull Request head is approved and ready for human review |
| `omnigrex:needs-human` | Autonomous progress stopped in a Human Handoff |

GitHub labels are the human-visible state, while PostgreSQL is the operational ledger.
Do not edit PostgreSQL records to force a transition.

## Retry And Review Budgets

An Agent Turn with no eligible successful terminal intent receives one infrastructure retry as a new Agent Turn in the same Agent Session.
If a unique terminal mutation intent succeeded before an ACP response was lost or the prompt deadline elapsed, Omnigrex can settle the turn successfully after corroborating the current Pull Request, review, or workspace state.
For a successful Developer or Reviewer outcome, the prompt failure remains in the settlement diagnostic; a turn without a successful terminal intent still follows the infrastructure retry policy.
Explicit cancellation and non-normal ACP stop reasons do not take this successful settlement path.
Infrastructure retries do not consume the review budget.
After the second failure, Omnigrex publishes diagnostics and creates a Human Handoff.

When `request_review` or `submit_review` succeeded and the ACP ending is eligible but a fresh corroboration read is unavailable, Omnigrex stops the Runtime Process and keeps the original Turn fenced while a separate worker retries the read. It does not prompt the agent or repeat the GitHub mutation. The default window is 30 minutes, configured by `OMNIGREX_TERMINAL_CORROBORATION_DURATION`. Its start is durable across orchestrator restarts; changing the configured duration changes the remaining window at the next check. The Issue retains its current developing/reviewing label while verification is pending. A clear access prerequisite or exhausted window creates a Human Handoff explaining that the mutation succeeded but the outcome was not accepted. Closing the Issue cancels pending verification.
If Runtime Process cleanup or the orchestrator fails after the ACP ending was atomically recorded with closed mutation admission, recovery first proves the process stopped and settles the mutation ledger before creating the original Turn's pending verifier.

After that specific Human Handoff, a new human `omnigrex:run` first revalidates the old intent without another agent prompt **when** the Stage and Role still match, the Assignment Generation has not been replaced, and the Agent Session is automation-controlled. The old failed settlement is not rewritten; a fresh read must still prove the current PR/review identity and Developer workspace tree. If the Session is human-controlled, the trigger remains in Human Handoff rather than starting an unpreparable Turn. If the Stage or Assignment Generation changed, normal reactivation applies instead of adopting the old intent. If the old ACP ending was **not** durably known because execution crashed earlier, ordinary infrastructure recovery gives the successor Agent Turn an explicit prior-intent notice. That agent can call `confirm_prior_terminal_intent` for the recorded source mutation; Omnigrex verifies it without submitting a duplicate review. A review submitted before a **known explicit cancellation** cannot be confirmed or resubmitted in the retry; the successor is instructed to report the blocker for Human Handoff. Mere Agent Session memory or a final text response is not a terminal intent.

Each Workflow Attempt permits at most three accepted Review Cycles.
A review counts only after the Reviewer App publishes it for the expected Pull Request head and that head remains current.
A stale review does not consume the budget.
The third accepted changes-requesting review creates a Human Handoff.

Re-adding `omnigrex:run` to the Issue or to the current Change Proposal creates a new Workflow Attempt, resets both budgets, and reuses retained Agent Sessions when possible.
Accepted Pull Request commands consume the trigger and reconcile state labels on both the Issue and the current Pull Request through the existing durable label worker.
Rejected Pull Request commands leave labels unchanged.
The default Agent Turn timeout is two hours, and the default global Agent Turn concurrency is two.

## Session Retention

Closing an Issue fences new work, settles admitted mutations, completes its current Agent Participants, and schedules Runtime Process state deletion.
The default retention period is 30 days and is configured with `OMNIGREX_ASSIGNMENT_RETENTION_DURATION`.
Reopening before garbage collection cancels scheduled deletion but does not restart automation.
Add `omnigrex:run` to the Issue or to the current Change Proposal after reopening to create a new Workflow Attempt.

Once physical collection starts, deletion is intentionally irrevocable.
After collection, a new trigger creates a new Assignment Generation whose Stage Assignments lazily select new Agent Participants and Agent Sessions, while PostgreSQL retains prior operational history.

Every active or retained Agent Session stays pinned to its original Runtime Profile image digest.
Keep every referenced digest available until the corresponding state is garbage-collected.

## Backup

PostgreSQL and `omnigrex-runtime-state` form one coordinated recovery set.
PostgreSQL is the authoritative execution ledger, while the runtime-state volume contains OpenCode session state needed for Session Continuation.
Do not copy runtime state while an Agent Turn is active.

Stop the orchestrator gracefully and verify that no managed Runtime Process remains:

```sh
docker compose stop -t 30 orchestrator
docker ps --filter label=io.omnigrex.runtime-process=true --format '{{.ID}} {{.Names}}'
```

The second command must produce no output before backup continues.
If a process remains, investigate and stop it before copying state.

Create a unique private recovery-set directory, then write each artifact atomically so a failed backup cannot replace a known-good recovery set:

```sh
(
  set -eu
  umask 077
  BACKUP_ROOT="$PWD/backups"
  BACKUP_DIR="$BACKUP_ROOT/$(date -u +%Y%m%dT%H%M%SZ)"
  install -m 0700 -d "$BACKUP_ROOT"
  mkdir -m 0700 "$BACKUP_DIR"
  cleanup_failed_backup() { rm -rf -- "$BACKUP_DIR"; }
  trap cleanup_failed_backup EXIT
  trap 'exit 1' HUP INT TERM

  docker compose exec -T postgres \
    pg_dump --username=omnigrex --dbname=omnigrex \
    --format=custom --no-owner --no-acl \
    > "$BACKUP_DIR/omnigrex.dump.tmp"
  docker compose exec -T postgres \
    pg_restore --list < "$BACKUP_DIR/omnigrex.dump.tmp" > /dev/null
  mv "$BACKUP_DIR/omnigrex.dump.tmp" "$BACKUP_DIR/omnigrex.dump"

  : "${BACKUP_DIR:?BACKUP_DIR must be set}"
  test -d "$BACKUP_DIR"
  HELPER='alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b'
  docker run --rm --network none --read-only \
    --mount type=volume,src=omnigrex-runtime-state,dst=/source,readonly \
    "$HELPER" sh -ec 'cd /source; tar -czf - .' \
    > "$BACKUP_DIR/runtime-state.tar.gz.tmp"
  tar -tzf "$BACKUP_DIR/runtime-state.tar.gz.tmp" > /dev/null
  mv "$BACKUP_DIR/runtime-state.tar.gz.tmp" "$BACKUP_DIR/runtime-state.tar.gz"

  (
    cd "$BACKUP_DIR"
    shasum -a 256 omnigrex.dump runtime-state.tar.gz > SHA256SUMS.tmp
    mv SHA256SUMS.tmp SHA256SUMS
  )

  trap - EXIT HUP INT TERM
  printf 'Recovery set: %s\n' "$BACKUP_DIR"
)
```

The archive preserves numeric ownership, including Runtime Process UID and GID 10001.
The workspace and mise volumes are reproducible caches and are not required for disaster recovery when no Agent Turn is active.
Archive them separately if local policy requires a complete host checkpoint.

Back up `.env`, secret files, compatibility artifacts, the deployed Git commit, and every referenced image digest separately in encrypted storage.
Never place those files in the same unencrypted backup directory as operational exports.

Restart the unchanged deployment after the backup:

```sh
docker compose up -d --wait orchestrator
```

## Restore

Restore only from a PostgreSQL dump and runtime-state archive created in the same stopped-orchestrator backup window.
The following procedure is destructive and replaces current operational and session state.

Restore the matching `.env`, secrets, application revision, compatibility artifacts, and exact Runtime Profile images before changing current data.
The database password secret must match the restored PostgreSQL role because `POSTGRES_PASSWORD_FILE` does not alter an existing role password.

Select the recovery-set directory once, verify it, stop the complete stack without deleting volumes, and restore both state stores in one fail-fast operation:

```sh
(
  set -eu
  BACKUP_DIR='/absolute/path/to/backups/20260907T120000Z'
  HELPER='alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b'

  (
    cd "$BACKUP_DIR"
    shasum -a 256 -c SHA256SUMS
  )
  docker compose down

  RUNTIME_PROCESS_IDS="$(docker ps --quiet --filter label=io.omnigrex.runtime-process=true)"
  if [ -n "$RUNTIME_PROCESS_IDS" ]; then
    printf 'Managed Runtime Processes remain after shutdown:\n%s\n' "$RUNTIME_PROCESS_IDS" >&2
    exit 1
  fi

  docker volume create omnigrex-runtime-state
  docker run --rm --network none --read-only \
    --mount type=volume,src=omnigrex-runtime-state,dst=/target \
    --mount "type=bind,src=$BACKUP_DIR,dst=/backup,readonly" \
    "$HELPER" sh -ec \
    'find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xzf /backup/runtime-state.tar.gz -C /target'

  docker compose up -d --wait postgres
  docker compose exec -T postgres dropdb --username=omnigrex --if-exists omnigrex
  docker compose exec -T postgres createdb --username=omnigrex --owner=omnigrex omnigrex
  docker compose exec -T postgres \
    pg_restore --username=omnigrex --dbname=omnigrex \
    --exit-on-error --no-owner --no-acl \
    < "$BACKUP_DIR/omnigrex.dump"
)
```

Start the full stack and run preflight for every repository.
Restore the matching `.env`, secrets, compatibility artifacts, and exact Runtime Profile images on the new host first; retained Agent Sessions cannot continue when their pinned digests are missing:

```sh
docker compose up --wait
docker compose exec orchestrator \
  /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

Perform restore drills in a non-production environment.
A complete drill resumes a retained test Work Item, observes the same Agent Session, and confirms History Replay after fresh containers.

Never use `docker compose down --volumes` during ordinary deployment, backup, or restore.
That command permanently removes all declared named volumes.

## Upgrades

Create a coordinated backup before every application, PostgreSQL, or Runtime Profile upgrade.

### Orchestrator-Only Upgrade

An orchestrator-only upgrade keeps the same OpenCode digest and compatibility artifact.
Record the current commit and image references, download the target commit's `deploy/compose.yaml` and `deploy/.env.example` into the deployment directory, keep `OMNIGREX_OPENCODE_ACP_V1_IMAGE` and the compatibility file unchanged, update `OMNIGREX_ORCHESTRATOR_IMAGE` to the target digest, validate Compose, and deploy:

```sh
docker compose config --quiet
docker compose up --wait
```

The orchestrator applies forward-only embedded PostgreSQL migrations under an advisory lock during startup.
There are no down migrations.
Do not downgrade an upgraded database in place.
Rollback after a migration requires restoring the coordinated pre-upgrade PostgreSQL and runtime-state backup together with the previous application revision and configuration.

For a PostgreSQL major-version upgrade, use a logical dump and restore into a new volume using the target PostgreSQL version.
Do not reuse a physical PostgreSQL data volume across major versions.

### Runtime Profile Image Change

Existing Agent Sessions remain pinned to the exact image that created them.
A source-to-candidate qualification does not migrate those sessions and does not prove candidate-to-source rollback compatibility.

Qualify the exact old-image to new-image upgrade in CI or another source checkout, not on the clone-free deployment host.
Packaging a host-side qualification tool is out of scope.
Pull both exact source and candidate image digests there, choose an absolute output path whose file does not yet exist, and run:

```sh
SOURCE='ghcr.io/jozala/omnigrex/opencode@sha256:SOURCE_DIGEST'
TARGET='ghcr.io/jozala/omnigrex/opencode@sha256:TARGET_DIGEST'
ARTIFACT="$PWD/runtime-profile-compatibility-results.json"
docker pull --platform=linux/amd64 "$SOURCE"
docker pull --platform=linux/amd64 "$TARGET"
OMNIGREX_PREVIOUS_OPENCODE_IMAGE="$SOURCE" \
OMNIGREX_PREVIOUS_OPENCODE_VERSION=1.18.19 \
OMNIGREX_OPENCODE_IMAGE="$TARGET" \
OMNIGREX_OPENCODE_VERSION=1.18.29 \
OMNIGREX_OPENCODE_ARCH=amd64 \
OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE="$ARTIFACT" \
mise run release-check-runtime-upgrade
```

Use the actual source and target versions.
The qualification creates the artifact with mode `0600`, and it refuses to overwrite an existing file.

Transfer the resulting JSON file to the deployment host (for example with `scp`), place it at an absolute host path outside the downloaded deployment directory, and set `OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE` to that path.
Set `OMNIGREX_OPENCODE_ACP_V1_IMAGE` to the candidate digest, pull that digest into the Docker Engine before startup, validate Compose, deploy, and run `doctor` for every repository.
Keep source and candidate digests available for every active and retained session.

An in-place Runtime Profile rollback is unsupported after Agent Participants using the candidate Runtime Profile exist unless the reverse direction has also been qualified.
The safe rollback is restoration of the coordinated pre-upgrade backup and previous deployment configuration.

## Manual Recovery

Omnigrex has no supported administrative command for editing Workflow records, requeueing internal jobs, or taking direct control of an Agent Session.
Do not edit PostgreSQL tables or runtime-state files manually.

Use this recovery sequence:

1. Read the `omnigrex:needs-human` comment and identify the failed prerequisite or uncertain mutation.
2. Correct GitHub App settings, repository access, credentials, images, storage, network access, or provider availability.
3. Recreate the orchestrator if configuration or image contents changed.
4. Run `doctor` until every check passes.
5. Confirm the Issue remains open.
6. Add `omnigrex:run` to create a new Workflow Attempt.

Re-adding the command label resets retry and review budgets but does not erase history.
Retained Agent Sessions are reused when their exact Runtime Profile image and state remain available.
If an earlier Developer Turn published a branch or opened a Pull Request but did not successfully request review, a later Turn for the same Agent Participant verifies that publication against its durable MCP mutations and fresh GitHub state before using it as in-progress work.
The later Turn starts at the verified published head and must explicitly request review; publication alone never schedules Reviewer.
An unproven branch, changed head, different Participant, or publication that cannot be matched to its recorded proposed commit (or, for older publications, its operation marker) causes a publication-conflict Human Handoff rather than automatic adoption.
If Git or GitHub cannot be reached during verification, execution follows bounded infrastructure retries instead of claiming a conflict.
Developer publication preserves committed Git history: an uncommitted workspace is rejected, while ignored local build artifacts are not published. A validated proposed commit is recorded before pushing; if an interrupted push cannot be corroborated on the branch, recovery does not reconstruct it from a later workspace and may require a Human Handoff.

For an apparently stuck deployment, inspect safe operational metadata:

```sh
docker compose ps
docker compose logs --since 15m orchestrator
docker ps --filter label=io.omnigrex.runtime-process=true \
  --format '{{.ID}} {{.Names}} {{.Status}}'
```

The orchestrator logs `MCP mutation failed` with a safe `failure_code`, operation and mutation identifiers, and allowlisted GitHub status/request ID or expected/observed commit IDs.
The corresponding `tool_invocations.last_error` contains the safe failure code and available allowlisted details for definite failures; unknown external outcomes remain subject to mutation reconciliation.
For read-tool failures, look for `MCP read failed` with the `agent_turn_id` and `tool_name`.
`failure_code` gives the broad classification, while `failure_stage`, `failure_reason`, and optional `validation_field` identify the processing step and specific failed check.
Detailed validation reasons initially cover issue lookup and review-thread reads; other read failures may have an `unclassified` reason.
For example, `github_invalid_response`, `review_threads_validation`, `missing_field`, and `review_thread.originalLine` identify a missing original line without disclosing a repository path or comment.
Cancellation and deadline expiry are classified before credential or GitHub errors are sanitized.

`github_http_status` is the actual status of the response associated with the failure, and `github_request_id` is its strictly validated GitHub request ID.
HTTP `200` does not imply GraphQL or tool success.
A missing status means response metadata is unavailable, not status `0` or proof that no request was sent.
A missing request ID can mean GitHub supplied none or its value failed validation.
Continuation failures use the response that exposed the failure, and final aggregate count mismatches use the last contributing response; earlier successful responses are never substituted for a request that received no response.
A scoped issue identity mismatch detected in the backend after a successful `GetIssue` has a precise `scoped_identity_validation` diagnostic but no response metadata because the successful-result interface does not carry it.

Authenticated shared gateway rejections emit `MCP request rejected`, including envelope, initialization, argument, authorization, and fence-validation failures.
Pre-dispatch failures can have no `tool_name`; arbitrary submitted tool names are never logged.
Use `agent_turn_id` to locate the Agent Turn, then filter by `diagnostic_call_id` to distinguish repeated calls and link a read failure to a subsequent recording failure.
Each rejected request emits one rejection entry; each failed read emits one execution entry and, if recording also fails, one additional entry at stage `read_recording` with the same ID.
Sending the final generic tool error does not add a duplicate rejection entry.
`duration_ms` measures elapsed processing from successful authentication to each log event, so the later recording entry can have a larger duration.
Keep diagnostic call IDs and GitHub request IDs as log fields, not Loki stream labels or metric labels.

`tool_invocations.last_error` and agent-facing read messages remain generic; these diagnostics never include arguments, GitHub response bodies, review text, or raw dependency errors.
`ACP MCP tool failed` is a runtime summary with a fixed `failure_class`, or `unclassified` for an unknown failure, rather than the authoritative backend cause.
ACP tool-call IDs and gateway diagnostic IDs are separate identities with no guaranteed direct mapping.
Runtime-local failures that never reach the gateway and authentication failures without trusted live registration scope remain blind spots.
Stale-authorization logs cover checks after successful authentication, not all expired or removed registrations.
Mutation-specific planning, reservation, replay, and detached execution are outside this read-diagnostic correlation guarantee and retain their existing diagnostics.

An Agent Turn diagnostic containing `runtime_oom_killed` means Docker confirmed that its Runtime Process was OOM-killed before Omnigrex removed the container.
Omnigrex keeps the normal single infrastructure retry, then includes this diagnosis in the Human Handoff if the retry also fails this way.
Inspect the affected Agent Turn and the retry separately, including their Runtime Profile binding, last tool calls, and available Docker or orchestrator logs, before deciding whether to start another Workflow Attempt.
An EOF or exit code 137 without Docker's `OOMKilled` state does not establish an OOM cause; when the container was removed before inspection, the cause remains unknown.
Do not change the immutable Runtime Profile binding of an existing Assignment to adjust its memory limit.

Do not publish logs without checking them for repository content and operational identifiers.
The application redacts known credential types, but operator-added proxy and platform logs may not.

Restarting the orchestrator is safe when using the same configuration and images:

```sh
docker compose restart orchestrator
```

Startup reconciliation fences stale Runtime Processes and resumes durable work according to retry policy.
Use `docker compose up -d --force-recreate orchestrator` instead when the image, environment, or mounted configuration changed.

If the Developer webhook receives no deliveries, inspect the Developer App's Recent deliveries page, public DNS and TLS, proxy logs, firewall, and exact path.
If deliveries receive 401, verify that the local and GitHub webhook secrets match.
If preflight reports a missing Runtime Profile image, pull the exact digest without replacing it with a mutable tag.
If PostgreSQL or runtime-state restoration is required, stop automation and use the coordinated restore procedure rather than retrying against mismatched state.

## Rotation

For a GitHub App private key, create a new key in GitHub, replace only that App's local PEM file, recreate the orchestrator, run preflight, and delete the old key after success.
Never rotate both App identities to the same key.

Webhook-secret rotation is not atomic across GitHub and the deployment.
Stop the orchestrator, update the GitHub App and local secret during one maintenance window, recreate the orchestrator, and verify a real delivery before starting new work.

For provider credentials, replace the deployment bundle, recreate the orchestrator, run preflight, and use a controlled test Work Item to verify every configured model.
Preflight checks JSON shape but cannot prove live provider authorization without making a paid request.

After any secret rotation, securely remove superseded local and backup copies according to the operator's retention policy.
