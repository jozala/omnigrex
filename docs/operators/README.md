# Operate Omnigrex

You deploy and maintain the control plane, credentials, execution resources, and retained data.
Repository owners configure their projects, and users interact through GitHub.
For a personal installation you may have all three responsibilities.

## Install

1. Read [requirements and the trust model](requirements.md).
2. Follow [installation](installation.md), including release selection, Apps, secrets, ingress, and backend provisioning.
3. Coordinate [repository onboarding](../repository-owners/connect-a-repository.md), run preflight, and verify a real webhook delivery.
4. Hand the ready repository to its users for a [first Work Item](../users/first-work-item.md).

## Maintain

| Task | Guide |
| --- | --- |
| Configure App permissions and installations | [GitHub Apps](github-apps.md) |
| Place or rotate credentials | [Secrets](secrets.md) |
| Understand execution and native storage constraints | [Backends](backends.md) |
| Supply deployment-owned agent instructions and approved tool paths | [Agent policy](agent-policy.md) |
| Verify readiness and diagnose onboarding | [Health and diagnostics](health-and-diagnostics.md) |
| Collect logs, traces, and application metrics | [Observability](observability.md) |
| Manage retained Sessions, images, and cache storage | [Retention](retention.md) |
| Create a coordinated recovery set | [Backup](backup.md) |
| Recover a compatible installation | [Restore](restore.md) |
| Change the controller or Runtime Profile image | [Upgrades](upgrades.md) |
| Investigate a failed or stuck deployment | [Troubleshooting](troubleshooting.md) |

Use [deployment configuration](../reference/deployment-configuration.md) and [commands](../reference/commands.md) for lookup.
Source-checkout diagnostics and release qualification belong in the [contributor guide](../contributors/runtime-qualification.md).

[Documentation hub](../README.md)
