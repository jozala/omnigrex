# Publish the website and documentation

[Operator guide](README.md) · [Local website build and preview](../../website/README.md)

The [Website GitHub Actions workflow](../../.github/workflows/website.yaml) builds the Astro homepage and Starlight documentation as one static artifact.
Relevant changes on `main` publish that artifact directly over SSH into the host directory already mounted by the website's Apache container.
The existing container, reverse proxy, hostname routing, and HTTPS certificate setup do not need to change.
Node and Docker are build tools on the runner, not production website dependencies.

## Prepare deployment access

Use the existing website login account and a separate SSH key for GitHub Actions, not your personal private key.
Generate and store the key outside the checkout, and install its public key in that account's `authorized_keys` using your normal server administration process.
Do not give this job root access or access to the Docker socket.
The key has the login account's permissions; it is not automatically confined to the website directory.

The GitHub-hosted runner must be able to reach the SSH server.
Both the runner and server need rsync 3 or newer; the deployment uses protected arguments for the remote path.
The login account needs write and delete access to the Apache host document root.
Do not deploy to the container-internal path: configure the directory on the host that is mounted there.

Obtain the server's SSH host public key through an already trusted connection or the VPS console and verify its fingerprint independently.
Keep the verified `known_hosts` entry for the exact configured hostname and port.
A nondefault port normally needs the `[hostname]:port` form.
The workflow uses strict host-key checking; it does not discover and trust a key during deployment.

Keep the SSH private key and deployment configuration out of commits, examples, artifacts, and application credentials.
The general secret-handling rules are in [Secrets](secrets.md).

## Configure GitHub

Create the repository environment **`website-production`** before merging the workflow.
Restrict its deployment branches to **`main`**.
Leave required reviewers disabled if publication should be automatic.

Set these environment variables:

| Variable | Value |
| --- | --- |
| `WEBSITE_SSH_HOST` | SSH hostname or address reachable by the runner |
| `WEBSITE_SSH_USER` | Existing website login account |
| `WEBSITE_SSH_PORT` | SSH port; empty uses `22` |
| `WEBSITE_DEPLOY_PATH` | Absolute host document-root directory, entirely managed by the website build |

Set these environment secrets:

| Secret | Value |
| --- | --- |
| `WEBSITE_SSH_KEY` | Complete private key for the dedicated CI identity, usable noninteractively |
| `WEBSITE_SSH_KNOWN_HOSTS` | Verified SSH host-key entry for the configured host and port |

Use a dedicated CI key that does not require an interactive passphrase; the workflow does not unlock passphrase-protected keys.
The deployment writes credentials into mode-0600 temporary files and removes them when the upload finishes or fails.
Credentials are supplied only to the deployment step, not Pull Request builds.

After the workflow has reported on a Pull Request, add **`Website check`** from GitHub Actions to the required status checks in the repository's `main` ruleset or branch protection.
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
