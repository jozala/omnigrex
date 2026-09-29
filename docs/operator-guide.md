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
- Allow request bodies up to 2 MiB.
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

When the Developer App gains access to a repository, Omnigrex provisions the
five managed labels before the first Workflow, using only Developer App
credentials:

- A new Developer App installation provisions every accessible repository.
  Accessible repositories are enumerated through a paginated
  installation-token request, not the webhook repository list.
- Explicitly added repositories are provisioned from the
  `installation_repositories.added` delivery.
- Repositories created under all-repository access are provisioned from the
  `repository.created` delivery.

Provisioning creates missing label names only, preserves the color and
description of existing labels, leaves unrelated labels untouched, and never
applies a state label to an Issue or Pull Request or starts a Workflow.
Duplicate or overlapping deliveries are harmless, one repository's failure
does not block other repositories, and terminal failures are recorded as
observable provisioning and delivery failures for operator follow-up.

Repositories installed before this provisioning was deployed are not
backfilled on startup. Their missing labels are created by the existing
Workflow label reconciliation the next time a Workflow runs. Labels deleted
from an idle repository are likewise restored only when a Workflow runs.

## Start A Workflow

Create a small Issue with explicit acceptance criteria and add the `omnigrex:run` label.
Omnigrex creates missing workflow labels when needed.

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

A failed or timed-out Agent Turn receives one infrastructure retry as a new Agent Turn in the same Agent Session.
If a unique terminal mutation intent succeeded before an ACP response was lost or the prompt deadline elapsed, Omnigrex can settle the turn successfully after corroborating the current Pull Request, review, or workspace state.
For a successful Developer or Reviewer outcome, the prompt failure remains in the settlement diagnostic; a turn without a successful terminal intent still follows the infrastructure retry policy.
Explicit cancellation and non-normal ACP stop reasons do not take this successful settlement path.
Infrastructure retries do not consume the review budget.
After the second failure, Omnigrex publishes diagnostics and creates a Human Handoff.

Each Workflow Attempt permits at most three accepted Review Cycles.
A review counts only after the Reviewer App publishes it for the expected Pull Request head and that head remains current.
A stale review does not consume the budget.
The third accepted changes-requesting review creates a Human Handoff.

Re-adding `omnigrex:run` creates a new Workflow Attempt, resets both budgets, and reuses retained Agent Sessions when possible.
The default Agent Turn timeout is two hours, and the default global Agent Turn concurrency is two.

## Session Retention

Closing an Issue fences new work, settles admitted mutations, completes its current Agent Participants, and schedules Runtime Process state deletion.
The default retention period is 30 days and is configured with `OMNIGREX_ASSIGNMENT_RETENTION_DURATION`.
Reopening before garbage collection cancels scheduled deletion but does not restart automation.
Add `omnigrex:run` after reopening to create a new Workflow Attempt.

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

  HELPER='alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b'
  docker run --rm --network none --read-only \
    --mount type=volume,src=omnigrex-runtime-state,dst=/source,readonly \
    --mount "type=bind,src=$BACKUP_DIR,dst=/backup" \
    "$HELPER" sh -ec 'umask 077; cd /source; tar -czf /backup/runtime-state.tar.gz.tmp .'
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

For an apparently stuck deployment, inspect safe operational metadata:

```sh
docker compose ps
docker compose logs --since 15m orchestrator
docker ps --filter label=io.omnigrex.runtime-process=true \
  --format '{{.ID}} {{.Names}} {{.Status}}'
```

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
