# Requirements and trust model

[Operator guide](README.md) · [Documentation hub](../README.md)

## Supported deployment

One Docker Compose control plane runs the orchestrator and PostgreSQL.
Disposable Pi Runtime Processes run on one installation-selected Docker or Microsandbox Execution Backend.
Pi is the only supported coding-agent runtime in current source and deployment artifacts.

Production hosts use published `linux/amd64` images and commit-pinned deployment files without cloning the repository.
Select a commit whose matching images passed publication and both-backend exact-digest deployment smoke.
A source build or synthetic fixture alone does not establish release qualification.

## Host prerequisites

- Docker Engine API 1.45 or newer and Docker Compose v2.
- Public DNS and publicly trusted HTTPS ingress for GitHub webhooks.
- `curl` for pinned deployment files, `openssl` for secret generation, `jq` for inspection, `tar` for archives, and `shasum` for recovery checksums.
- Capacity for the orchestrator, PostgreSQL, admitted Agent Turns, tool compilation, storage growth, and any native VMM overhead.
- For Microsandbox: Linux/amd64, usable `/dev/kvm`, the native control-plane image, and its device group.

Git and mise are not required on the no-clone deployment host.
Runtime Profile qualification uses a separate source checkout.
Docker-backed development on macOS is supported with locally built arm64 images; published deployment images are initially amd64.
See [backend requirements](backends.md) before choosing the installation backend.

## Trust and credentials

Only trusted repositories are supported, including dependencies, plugins, instructions, and commands.
The Docker control plane has effective host-root authority through the Docker socket and can read deployment secrets.
The Microsandbox control plane receives KVM access instead of the host Docker socket.

Pi Runtime Processes receive no GitHub credentials or host Docker socket.
They receive the selected provider's real API key, or controller-issued access-only ChatGPT tokens, and outbound network access.
Malicious repository code can expose supplied credentials even inside a microVM.
Different Roles can select different providers from one bundle; only the selected entry enters each environment.
Scope provider privileges narrowly, configure spending limits, and rotate the complete bundle after suspected compromise.
Credential mediation is not implemented.
ChatGPT refresh credentials remain controller-owned; see [provider authentication](provider-authentication.md) for login, renewal, and account-wide limits.

Local tool permissions, including Reviewer edit restrictions, do not turn arbitrary trusted shell execution into a file-security boundary.
Workflow transitions and MCP authority remain separately enforced.
GitHub required checks are not enforced before `omnigrex:pr-ready`; repository owners must configure branch protection for mandatory checks.

## Supported release baseline

The current baseline uses `pi-rpc/v2`, guest tool storage under `/home/omnigrex`, and the clean initial database schema.
Initialize it with fresh PostgreSQL, runtime-state, workspace, and tool volumes; Microsandbox also needs a fresh native home and namespace.
Startup rejects incompatible migration history and unsupported Runtime Profile contracts without converting or deleting deployment state.
This is the [clean supported baseline](../decisions/0024-establish-a-clean-supported-release-baseline.md), not an in-place conversion from old OpenCode installations.

Retained Participants keep exact image, platform, resource, backend, and storage bindings.
Docker recovery also requires the current management marker and exact execution identity; identity labels on an unmarked container do not authorize adoption or cleanup.
Keep protected images and matching state available through upgrades and recovery.

Next: [Install Omnigrex](installation.md).
