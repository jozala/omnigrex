# Place and rotate secrets

[Operator guide](README.md) · [Installation](installation.md)

Start in the private deployment directory with the selected `COMPOSE_FILE` exported.
Create `secrets/` with mode 0700 if the installation procedure has not already done so.
Keep `.env`, private keys, provider bundles, compatibility artifacts containing deployment metadata, and backups out of source control.

## Create secret files

Generate independent database and webhook secrets:

```sh
openssl rand -base64 32 > secrets/database-password
openssl rand -hex 32 > secrets/github-webhook-secret
```

Copy each App key into the separate PEM path in the [App guide](github-apps.md#keys-and-installation).
Copy a Pi-native API-key bundle:

```sh
cp /secure/path/pi-auth.json secrets/provider-credentials.json
chmod 0640 secrets/*
```

The bundle is a nonempty JSON object with literal API keys:

```json
{
  "provider-name": {
    "type": "api_key",
    "key": "REPLACE_WITH_SECRET"
  }
}
```

The requested `provider/model` must exist in the exact Pi image and work with that key.
Executable key references, environment references, OAuth entries, and missing selected providers are unsupported.
Only the selected entry is written to mode-0600 `/run/pi-agent/auth.json` in disposable tmpfs; MCP bearer authority stays in memory.
Normal retained-state, database, image, and diagnostic paths exclude keys.
Trusted repository code can deliberately copy supplied keys, so this is not credential mediation.

## Secret file group

For Docker on Linux, obtain the socket's numeric group:

```sh
stat -c '%g' /var/run/docker.sock
```

Set `OMNIGREX_DOCKER_GID` to that number, or `0` for Docker Desktop.
Microsandbox instead uses the KVM device group as described in [installation](installation.md#microsandbox-bundle).

Create a dedicated host group using the operating system's account-management tools.
Change `secrets/*` to that group and set its numeric ID as `OMNIGREX_SECRET_GID`.
It must differ from the selected backend's socket or KVM group, and unrelated users must not belong to it.
The controller runs as UID/GID 10001 and receives supplementary groups through Compose.
Socket group membership has host-root authority; mounting the socket read-only does not restrict API operations.

## Verify credentials

Run repository [doctor](health-and-diagnostics.md#repository-preflight) after placing or changing secrets.
Preflight checks configuration and prerequisites without a paid provider prompt; it cannot prove live provider authorization.
Use a controlled Work Item to verify actual selected models after credential changes.
The separate [live-provider gate](../contributors/runtime-qualification.md#live-provider-smoke) is an opt-in source-checkout diagnostic.

## Rotation

For a GitHub App key, generate a new key, replace only that App's local PEM file, recreate the controller, run preflight, and delete the old key after success.
Never rotate both App identities to the same key.

Webhook-secret rotation is not atomic across GitHub and the deployment.
Stop the controller, change the remote and local secret during one maintenance window, recreate the controller, and verify a real delivery before new work.

For provider credentials, replace the complete deployment bundle, recreate the controller, run preflight, and verify every configured model with controlled work.
After suspected repository compromise, rotate the complete provider bundle.
Securely remove superseded local and backup copies according to the operator's retention policy.

Configuration changes require recreation with the same Compose file set:

```sh
docker compose up -d --force-recreate orchestrator
```

Next: [Installation verification](installation.md#7-verify-and-hand-over).
