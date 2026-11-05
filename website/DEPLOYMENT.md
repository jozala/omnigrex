# Publish the website and documentation

[Local website build and preview](README.md)

This procedure maintains `omnigrex.com`, not an Omnigrex application installation.
The [Website GitHub Actions workflow](../.github/workflows/website.yaml) builds the Astro homepage and Starlight documentation as one static artifact.
Relevant changes on `main` publish that artifact directly over SSH into the host directory already mounted by the website's Apache container.
The existing reverse proxy, hostname routing, and HTTPS certificate setup do not need to change.
Node and Docker are build tools on the runner, not production website dependencies.

## Prepare deployment access

Use a dedicated deployment account and a separate SSH key for GitHub Actions, not your personal login account or private key.
Generate and store the key outside the checkout.
The account must have no sudo, Docker-group, or Docker-socket access and should have write permission only to the website document root, outside any personal home directory.
Use a minimal shell such as `/bin/dash` to execute the forced command; `nologin` prevents that command from running, while Bash startup files introduce avoidable execution paths.
Keep the account's home, `.ssh` directory, and `authorized_keys` root-owned and unwritable by the deployment account.
The public-key file must remain readable by the account; mode `0644` is appropriate for a root-owned `authorized_keys` file.

Install the public key with a forced [rrsync](https://download.samba.org/pub/rsync/rrsync.1) command, using the installed wrapper's absolute path and the fixed host document root.
For example, the following is one physical `authorized_keys` line with a placeholder public key and document root:

```text
restrict,command="/usr/bin/rrsync -wo -munge /srv/website" ssh-ed25519 REPLACE_WITH_PUBLIC_KEY
```

`restrict` disables PTY allocation, forwarding, and user SSH startup commands.
`-wo` allows uploads and deletion within the selected directory but rejects downloads.
`-munge` makes uploaded symlinks unusable, so they cannot redirect access outside that directory.
The server enforces the directory restriction; a client-side destination check alone would not constrain a compromised deployment key.
Revoke this key's authorization on any personal or unrestricted account after testing the restricted account, keeping a separate administrative session open until personal login has been verified.
Configure Apache to mount the selected document root read-only; recreate an existing container if its bind mount changes.

The GitHub-hosted runner must be able to reach the SSH server.
Use maintained standard rsync 3 or newer on the runner and server, with `rrsync` installed on the server.
macOS's bundled openrsync is not compatible with this wrapper's command parser; use standard rsync for local connection tests.
The account needs write and delete access to the Apache host document root, including permission to update its directory metadata.
The client always uploads to `:./`, which the forced command resolves inside the selected directory.
There is no configurable client destination path, and an obsolete `WEBSITE_DEPLOY_PATH` value is ignored.
Do not add `--protect-args` or `--secluded-args`: `rrsync` rejects them because it must inspect the requested paths and options.

Obtain the server's SSH host public key through an already trusted connection or the VPS console and verify its fingerprint independently.
Keep the verified `known_hosts` entry for the exact configured hostname and port.
A nondefault port normally needs the `[hostname]:port` form.
The workflow uses strict host-key checking; it does not discover and trust a key during deployment.

Keep the SSH private key and deployment configuration out of commits, examples, artifacts, and application credentials.
The general secret-handling rules are in [Secrets](../docs/operators/secrets.md).

## Configure GitHub

Create the repository environment **`website-production`** before merging the workflow.
Restrict its deployment branches to **`main`**.
Leave required reviewers disabled if publication should be automatic.
Store the following variables and secrets in this environment, rather than repository-wide Actions settings, to limit credential availability to deployment jobs.
Repository-wide settings also resolve in this workflow, but migrate and remove duplicate website entries to avoid leaving broader access or confusing fallback values.

Set these environment variables:

| Variable | Value |
| --- | --- |
| `WEBSITE_SSH_HOST` | SSH hostname or address reachable by the runner |
| `WEBSITE_SSH_USER` | Dedicated account restricted to the website root through `rrsync` |
| `WEBSITE_SSH_PORT` | SSH port; empty uses `22` |

Delete the former `WEBSITE_DEPLOY_PATH` variable when migrating an existing setup.
Choose the document root only in the server's forced command, never in GitHub configuration.

Set these environment secrets:

| Secret | Value |
| --- | --- |
| `WEBSITE_SSH_KEY` | Complete private key for the dedicated CI identity, usable noninteractively |
| `WEBSITE_SSH_KNOWN_HOSTS` | Verified SSH host-key entry for the configured host and port |

Use a dedicated CI key that does not require an interactive passphrase; the workflow does not unlock passphrase-protected keys.
The deployment writes credentials into mode-0600 temporary files and removes them when the upload finishes or fails.
It ignores SSH client configuration and agents, uses only the supplied identity, and verifies the server against the supplied host-key file.
Credentials are supplied only to the deployment step, not Pull Request builds.

Push the feature branch and open a Pull Request so the workflow can report **`Website check`** before the changes are merged.
After that check has run successfully, add **`Website check`** from GitHub Actions to the required status checks in the repository's `main` ruleset or branch protection.
For a ruleset, open **Settings → Rules → Rulesets**, edit the rule targeting `main`, enable **Require status checks to pass**, and select **`Website check`**.
For classic protection, open **Settings → Branches**, edit the protection rule for `main`, and add it under **Require status checks to pass before merging**.
The check does not need to be merged into `main` first, but it needs a recent successful result to appear in GitHub's check picker.
GitHub requires a successful result within the past seven days; see [troubleshooting required status checks](https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks).
Keep the existing product `check` requirement.
Require the aggregate `Website check`, not the conditional `Build website` job: unchanged website inputs intentionally skip that build.
The aggregate still reports success for an unchanged site and fails if change detection or a required build fails, is cancelled, or is unexpectedly skipped.
Repository settings are an operator action, not a side effect of running the workflow.

## Publish and redeploy

Merge a qualified change affecting published documentation, website sources, assets, or website build/deployment configuration into `main`.
The workflow tests and builds the site, uploads the generated output as an artifact, and downloads that artifact by its build-provided ID in the environment-scoped deployment job.
Rerunning only a failed deployment reuses the successful build's artifact instead of assuming an artifact exists for the new attempt number.
Uploads are serialized and an active upload is not cancelled for a newer run.
Website publication does not wait for application/KVM qualification or publish application images.

For a manual redeployment, open **Actions → Website → Run workflow**, choose **`main`**, and run it.
Manual dispatch builds even if no relevant files changed.
Rerunning an older workflow run republishes that run's revision; use manual dispatch on `main` to publish the current revision instead.

`rsync --delete` makes the destination match the generated artifact, including removing obsolete pages and files.
Do not keep unrelated files in that directory.
There is no staging directory, atomic switch, release retention, backup, automatic rollback, or public HTTP smoke check.
Visitors may see a partially updated site during upload, and an interrupted upload can leave partial publication until a later successful upload.
This is the explicitly selected publication policy, not an application deployment or recovery mechanism.

## Diagnose a failure

Check the failed job in GitHub Actions.
Pull Request/build failures cannot start the deployment job.
If upload fails, correct the credentials, host key, SSH reachability, directory permissions, or rsync availability indicated by its error, then manually dispatch on `main` again.
Artifacts are retained for seven days; after expiry, use a new manual dispatch rather than retrying an old deployment.
For a host-key change, independently verify the new fingerprint before updating `WEBSITE_SSH_KNOWN_HOSTS`; do not disable host-key checking.

A successful deployment job means rsync completed successfully.
It does not independently verify public HTTP responses, proxy configuration, DNS, TLS, or a running Apache container.
