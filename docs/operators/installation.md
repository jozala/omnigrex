# Install Omnigrex

[Operator guide](README.md) · [Documentation hub](../README.md)

This procedure deploys published images without a source checkout.
First read [requirements and the trust model](requirements.md) and choose one [backend](backends.md).
Use a fresh installation for the supported baseline.

## 1. Select a qualified commit

Select a stable [GitHub Release](https://github.com/jozala/omnigrex/releases) recommended for users, or explicitly opt into an alpha prerelease.
Both are pre-1.0: stable does not promise production readiness or permanent backward compatibility.
Read its human notes, breaking-change declaration, upgrade requirements, and `release.json` before proceeding.
Use a fresh installation unless a compatible upgrade path and its manual prerequisites have been established.
See [release publication](../contributors/releases.md) for candidate selection and retained qualification evidence.

## 2. Stage deployment files

Download `payload.tar.gz`, `candidate.json`, `SHA256SUMS`, and `release.json` from the **same Release** into an empty directory; no checkout is needed.
For example, after replacing `VERSION` with the selected version:

```sh
VERSION=0.1.0
mkdir omnigrex-pi-deploy
cd omnigrex-pi-deploy
for asset in payload.tar.gz candidate.json SHA256SUMS release.json; do
  curl -fSLO "https://github.com/jozala/omnigrex/releases/download/v${VERSION}/${asset}"
done
sha256sum -c SHA256SUMS
tar -xzf payload.tar.gz
cat build.json
cd deploy
cp .env.pi.example .env
chmod 0600 .env
install -m 0700 -d secrets
export COMPOSE_FILE=compose.pi.yaml:compose.pi-docker.yaml
```

For Microsandbox export `COMPOSE_FILE=compose.pi.yaml:compose.pi-microsandbox.yaml` instead.
The archive contains both overlays, the common Compose file, environment example, and channel-independent build identity/product digests.
It contains no secrets, installation `.env`, private compatibility evidence, or backups.
`candidate.json` records the exact source commit, CI run/attempt and qualification job IDs; `release.json` records alpha/stable identity separately.
Checksums detect corruption, not an independent signature; trust GitHub repository access controls and the selected Release.

Keep this exact `COMPOSE_FILE` exported in every deployment, diagnostic, rotation, backup, and restore shell.
Append storage or instruction overrides to the same file set.
Leaving it unset can omit the overlay or select a source-built developer stack in a checkout.
The default volume/network names are fixed: another project name alone does not isolate another installation.
Parallel installations must override every storage/network name and matching volume, agent-network, and native namespace setting.

## 3. Resolve and provision images

Read the exact product digest references from `../build.json` (`images.orchestrator`, `images.orchestrator-microsandbox`, and `images.pi`).
Set `OMNIGREX_ORCHESTRATOR_IMAGE` to the selected backend controller, `OMNIGREX_PI_IMAGE` to generic Pi, and `OMNIGREX_PI_PLATFORM=linux/amd64` in the private `.env`.
Pull those **exact digest references**, not the mutable commit tags:

```sh
docker pull --platform=linux/amd64 'ghcr.io/jozala/omnigrex/orchestrator@sha256:REPLACE_FROM_BUILD_JSON'
docker pull --platform=linux/amd64 'ghcr.io/jozala/omnigrex/pi@sha256:REPLACE_FROM_BUILD_JSON'
```

For Microsandbox use the `orchestrator-microsandbox` digest and also provision its native cache in step 6.
No `latest` image tag is published.
All product images are Linux/amd64.
Package visibility must support anonymous pulls; contact the package administrator if a no-login pull fails, rather than inserting registry credentials in the bundle.
The `development` image is Omnigrex's own repository environment, **not a product release artifact** or a general environment required by other repositories.
Its adoption through `.omnigrex/environment.yaml` is independent of product release publication and does not change retained Participant bindings.

## 4. Configure Apps, secrets, and ingress

Complete these focused procedures before startup:

1. [Create the GitHub Apps](github-apps.md) and install both on the managed repositories.
2. [Place secrets and set file groups](secrets.md), and select [API-key or ChatGPT subscription authentication](provider-authentication.md).
3. [Configure HTTPS ingress](#https-ingress).
4. If required, add [operator agent guidance](agent-policy.md) and storage overrides to the selected Compose file set.
5. Coordinate [repository configuration](../repository-owners/connect-a-repository.md) with each owner.

Exported shell variables override `.env` values.
Inspect the effective controller selection without printing secrets:

```sh
docker compose config --format json |
  jq '.services.orchestrator | {image, backend: .environment.OMNIGREX_EXECUTION_BACKEND}'
docker compose config --quiet
```

## 5. Size the installation

The bundles default to two concurrent Agent Turns with 1 GiB per execution environment.
Allow additional capacity for the orchestrator, PostgreSQL, repository tool compilation, and native VMM overhead.
The Microsandbox example uses a 6 GiB control-plane limit for two 1 GiB Turns.
Set VM root capacity and CPU settings for the repository workloads rather than treating example values as capacity guarantees.

`OMNIGREX_PI_STARTUP_TIMEOUT` defaults to `10m` and bounds repository bootstrap, initial Pi configuration, and initial RPC readiness from Runtime Process attachment.
A shorter Turn or diagnostic deadline wins.
After the first validated `get_state`, subsequent configuration RPCs use ordinary 30-second deadlines while preparation remains bounded by the overall startup budget.
The overall Turn timeout defaults to `2h`, begins before Runtime Process launch, and includes startup and execution.
Increasing it alone does not extend startup readiness; stop/capture cleanup has a separate budget.
See [deployment settings](../reference/deployment-configuration.md) for configuration layers.

## 6. Start the selected bundle

### Docker bundle

Set the socket and secret groups using [secret file groups](secrets.md#secret-file-group), then start:

```sh
docker compose up -d --no-build --wait
docker compose exec orchestrator /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

The controller alone receives the host Docker socket.
The bundle needs no KVM device or SDK libraries.
For Docker-backed local macOS development, see [source development](../contributors/development.md).

### Microsandbox bundle

Set `OMNIGREX_MICROSANDBOX_KVM_GID` to the numeric group of `/dev/kvm` and use a distinct secret-file group.
The native overlay grants KVM access without mounting the host Docker socket, privileged mode, or extra Linux capabilities.
Replace the image placeholder below with the same exact Pi digest configured in `.env`:

```sh
docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator image pull 'REGISTRY/PI@sha256:DIGEST'
docker compose up -d --no-build --wait
docker compose exec orchestrator /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

The native backend rejects missing KVM and incompatible exact image/resources before serving webhooks or admitting Turns.
Protected native home holds the cache and dispatch journal; disk-backed `TMPDIR` there supports image preparation without overflowing tmpfs.
Preserve its protected journal in [backups](backup.md), not disposable cached images or VM disks.

## 7. Verify and hand over

For subscription authentication, complete [ChatGPT login](provider-authentication.md#sign-in) before requiring repository doctor to pass.
Subscription access is an execution prerequisite; a missing login does not prevent starting the controller to perform authentication.

Check local health and dependency readiness, using the configured HTTP port if it differs:

```sh
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

Run repository doctor for every managed repository and require every line to report `PASS` with exit status 0.
Use the Developer App's Advanced page to inspect or redeliver a real webhook and require HTTP `202 Accepted`.
Doctor cannot prove that GitHub's remote secret, public DNS, TLS, firewall, and proxy work together.
Wait for the trigger label and complete the [repository readiness handoff](../repository-owners/connect-a-repository.md#confirm-readiness).
Only then ask a user to add `omnigrex:run` to a small Work Item.

## HTTPS ingress

The Compose stack publishes its HTTP server on `${OMNIGREX_HTTP_PORT:-8080}` and keeps MCP private.
For a host-level reverse proxy, bind the backend to loopback in `.env`:

```dotenv
OMNIGREX_HTTP_PORT=127.0.0.1:8080
```

The proxy must terminate publicly trusted TLS, preserve the raw body and `X-Hub-Signature-256`, `X-GitHub-Delivery`, and `X-GitHub-Event` headers, and forward `POST /webhooks/github` without changing its path.
Allow bodies up to 25 MiB and complete request headers within five seconds and the request within fifteen seconds.
Keep port 8081 and the agent network private.

A minimal host-level Caddy configuration is:

```caddyfile
omnigrex.example.com {
  handle /webhooks/github {
    reverse_proxy 127.0.0.1:8080
  }

  respond 404
}
```

If the proxy is containerized, use the backend's reachable container address instead of the host-level loopback example.
Restrict direct HTTP-port access with a host firewall when loopback binding is unavailable.
Omnigrex does not consume forwarded-client headers or require the proxy to supply them.

Next: [Health and diagnostics](health-and-diagnostics.md), then [backup](backup.md).
