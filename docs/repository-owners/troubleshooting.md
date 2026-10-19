# Troubleshoot repository setup

[Repository-owner guide](README.md) · [Documentation hub](../README.md)

Start with the Human Handoff explanation or the operator's repository preflight output.
Publish fixes to the authoritative revision, then coordinate preflight before [reactivating work](../users/human-handoffs.md).

| Symptom | What to check |
| --- | --- |
| `omnigrex:run` is missing | Both App installations, Developer webhook delivery, asynchronous provisioning, and the operator's [label diagnostics](../operators/health-and-diagnostics.md#label-provisioning) |
| Profile discovery fails | Every direct `.omnigrex/team/*.md` file, strict front matter, unique names, known Roles, and exactly one Profile per required Role |
| Model or thinking is rejected | Exact provider/model support in the selected image, supported thinking level, and matching operator credentials |
| A required skill is missing | Physical skill file, `read` and `skill` permissions, and the actual Change Proposal checkout as well as the default branch |
| Environment configuration fails | Exact registry digest, supported platform/capabilities, schema, and operator provisioning into the selected backend |
| A build fails although doctor passed | Working-revision mise configuration, network access, actual project tests, memory, and scratch capacity; doctor does not run the full build |
| Tool path configuration fails | Operator allowlist, supported variable names, directory references, and lifecycle values |
| Configuration change conflicts with a retained Session | Existing immutable Runtime Profile binding; ask the operator rather than editing state |
| Push or comment did not resume work | Explicitly re-add `omnigrex:run` after resolving the Human Handoff; pushes alone do not resume it |

## Request operator help

Supply the repository, Work Item and Change Proposal links, relevant commits, the configuration file involved, and sanitized diagnostics.
Ask the operator to verify App access, exact images, provider availability, storage, and [health checks](../operators/health-and-diagnostics.md).
Do not add installation credentials to a repository configuration file to bypass a prerequisite failure.

## Verify the fix

The operator should obtain all-pass preflight and verify delivery when ingress or App settings changed.
For an environment change, qualify the project's actual build/tests in an isolated Work Item before promotion.
Keep existing image digests available for retained Participants.
Then reactivate an open Work Item explicitly and inspect its new attempt's outcome.
