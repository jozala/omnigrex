# Operator guide relocation index

<a id="operator-guide"></a>

The handbook has moved into focused [operator guides](operators/README.md), [repository-owner guides](repository-owners/README.md), [user guides](users/README.md), and [contributor guides](contributors/README.md).
Start at the [documentation hub](README.md) to choose your audience.
This page preserves older section links; authoritative procedures live at the destinations below.

<a id="supported-deployment"></a>
<a id="supported-release-baseline"></a>
<a id="backend-selection-and-guarantees"></a>
<a id="prerequisites"></a>

## Requirements

See [supported hosts, trust model, and release baseline](operators/requirements.md) and [backend selection](operators/backends.md).

<a id="pi-operational-deployment"></a>
<a id="published-images-and-deployment-files"></a>
<a id="deploy"></a>
<a id="docker-bundle"></a>
<a id="microsandbox-bundle"></a>
<a id="https-ingress"></a>

## Installation

See [image-only installation](operators/installation.md), [Docker](operators/installation.md#docker-bundle), [Microsandbox](operators/installation.md#microsandbox-bundle), and [HTTPS ingress](operators/installation.md#https-ingress).

<a id="microsandbox-execution-backend"></a>

## Native execution

See [backend requirements, images, storage, and evidence limits](operators/backends.md).

<a id="github-apps"></a>
<a id="developer-app"></a>
<a id="reviewer-app"></a>
<a id="keys-and-installation"></a>

## GitHub identity

See [App permissions and installation](operators/github-apps.md).

<a id="secrets"></a>
<a id="secret-file-group"></a>
<a id="rotation"></a>

## Credentials

See [secret placement, groups, and rotation](operators/secrets.md).

<a id="agent-profiles"></a>
<a id="instruction-ownership-and-operator-guidance"></a>
<a id="repository-skills"></a>
<a id="agent-turn-tool-paths"></a>
<a id="git-identity-and-review-handoffs"></a>
<a id="agent-participant-signatures"></a>

## Agent configuration

See [Profiles](repository-owners/configure-agents.md), [skills](repository-owners/repository-skills.md), [tool paths](repository-owners/tool-paths-and-caches.md), [operator guidance](operators/agent-policy.md), and [Git identity/review artifacts](reference/repository-configuration.md#git-identity-and-review-artifacts).

<a id="runtime-profile-image"></a>
<a id="repository-environments"></a>
<a id="omnigrex-development-environment"></a>
<a id="agent-turn-memory"></a>

## Environments and capacity

See [repository environments](repository-owners/development-environments.md), [the Omnigrex development image](contributors/development-image.md), [deployment image selection](operators/installation.md#3-resolve-and-provision-images), and [memory allocation](operators/retention.md#agent-turn-memory).

<a id="diagnostics-and-backend-gates"></a>
<a id="automatic-label-provisioning"></a>
<a id="manual-recovery"></a>

## Operational diagnostics

See [health, doctor, and label provisioning](operators/health-and-diagnostics.md), [troubleshooting](operators/troubleshooting.md), and [contributor test gates](contributors/testing.md).

<a id="tracing"></a>
<a id="application-metrics"></a>

## Telemetry

See [logs, traces, and metrics](operators/observability.md).

<a id="start-a-workflow"></a>
<a id="retry-and-review-budgets"></a>

## Everyday use

See [your first Work Item](users/first-work-item.md), [Workflow labels and budgets](users/workflow-and-labels.md), and [Human Handoffs](users/human-handoffs.md).

<a id="session-retention"></a>
<a id="backup"></a>
<a id="supplemental-session-exports"></a>
<a id="restore"></a>
<a id="upgrades"></a>
<a id="orchestrator-only-upgrade"></a>
<a id="runtime-profile-image-change"></a>

## Data maintenance

See [retention](operators/retention.md), [backup and export](operators/backup.md), [restore](operators/restore.md), and [upgrades](operators/upgrades.md).

<a id="pi-protocol-diagnostic"></a>
<a id="live-provider-smoke"></a>
<a id="pi-agent-turn-qualification"></a>
<a id="candidate-pi-agent-turn-qualification"></a>
<a id="pi-upgrade-and-state-recovery-qualification"></a>
<a id="compatible-host-pi-restore-drills"></a>

## Contributor qualification

See [protocol, provider, Workflow, recovery, and upgrade qualification](contributors/runtime-qualification.md).
