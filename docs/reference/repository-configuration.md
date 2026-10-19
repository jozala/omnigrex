# Repository configuration reference

[Reference](README.md) · [Repository-owner guide](../repository-owners/README.md)

## File authority

| Path | Source revision | Purpose |
| --- | --- | --- |
| `.omnigrex/team/*.md` | One pinned latest default-branch commit before each Turn | Required Agent Profiles |
| `.omnigrex/environment.yaml` | Same commit as Profiles | Optional image/platform/capability request for new Participants |
| `.omnigrex/turn-configuration.yaml` | Same commit as Profiles | Optional disk-backed tool-path requests for both Roles |
| `.agents/skills/<name>/SKILL.md` | Prepared working checkout | Explicitly authorized engineering procedures |
| `mise.toml` and its scripts/hooks | Actual working checkout | Repository tool installation and activation |

Default-branch configuration authority does not imply default-branch skills or tool installation during Change Proposal review.
Unknown configuration is rejected rather than silently ignored.

## Agent Profiles

| Field | Contract |
| --- | --- |
| `name` | Stable repository-local identity, lowercase letters/digits/hyphens/underscores |
| `role` | Role ID used by the Workflow Definition, initially `DEVELOPER` or `REVIEWER` |
| `runtime` | `pi-rpc/v2` |
| `model` | Exact `provider/model` supported by the image and supplied credentials |
| `thinking` | Optional `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`; vocabulary validity is distinct from model-specific support, verified against the actual Pi runtime |
| `permissions` | Nonempty map of supported local permission names to `allow` or `deny` |

Supported local permissions are `read`, `edit`, `write`, `bash`, `grep`, `find`, `ls`, and `skill`.
Omitted permissions default to deny; Reviewer `edit`/`write` cannot be allowed.
MCP capabilities are authorized independently.
Delegation, unknown Profile fields, unknown permissions, and per-step limits are unsupported.
Every direct lowercase Markdown child of `.omnigrex/team/` is treated as configuration.
The current selector requires exactly one Profile per Workflow Role.

`max` is Pi's native maximum reasoning level and is distinct from `xhigh`; neither is substituted for the other.
A valid vocabulary value passes discovery, but the selected model must also advertise it through `get_available_thinking_levels`.
Unsupported or silently clamped selections fail during runtime preparation and doctor before any prompt is admitted, without fallback or downgrade.

The body follows strict YAML front matter and holds preferences/instructions.
Filename changes preserve identity when `name` remains unchanged.
Do not put secrets or a directory README among the Profiles.
See [configuration examples](../repository-owners/configure-agents.md).

## Environment

| Field | Contract |
| --- | --- |
| `version` | `1` |
| `image` | Exact untagged registry `@sha256:` digest |
| `platform` | Supported Linux guest platform, `linux/amd64` or `linux/arm64`; native deployment requires amd64 |
| `requirements` | Backend-supported capability names |

Docker accepts `process-isolation` and `tool-installation`.
Microsandbox additionally accepts `hardware-isolation` and `nested-containers`.
An absent file selects the deployment golden image for new Participants.
Participants retain immutable exact contracts and do not rebind after edits.
See [environment construction and promotion](../repository-owners/development-environments.md).

## Tool paths

`version: 1` declares `environment-paths.directories` and `environment-paths.environment`.
Each directory name matches `[a-z][a-z0-9-]{0,63}` and specifies `lifecycle: turn` or `assignment`.
Each environment entry has a unique approved `name` and references a declared `directory`.
The nonempty file is limited to 64 KiB, at most 64 directories and 128 environment entries, and exactly one YAML document.
Variable names use shell identifier syntax, at most 128 characters, and must be operator-allowlisted.

Reserved prefixes include `PI_`, `OMNIGREX_`, `MISE_`, `XDG_`, `LD_`, `DYLD_`, and `GIT_`.
Reserved individual variables include `PATH`, `HOME`, `ENV`, `BASH_ENV`, `SHELLOPTS`, `PYTHONPATH`, `PYTHONHOME`, `NODE_OPTIONS`, `BUN_OPTIONS`, `JAVA_TOOL_OPTIONS`, and `SSL_CERT_FILE`.
The exact validator is [internal/turnconfig/config.go](../../internal/turnconfig/config.go).
See [tool-path examples and lifecycle](../repository-owners/tool-paths-and-caches.md).

## Git identity and review artifacts

Workspace preparation sets repository-local `user.name=Omnigrex` and `user.email=agent@omnigrex.invalid` without changing global configuration or existing commits.
New Participant comments, reviews, and inline findings receive the visible Profile/Role signature; the hidden idempotency marker follows it.

`request_review` publishes a signed PR summary identifying the exact head before its terminal handoff succeeds.
This applies to initial and requested-changes Turns; agents should not duplicate the summary or reopen an existing PR.
The successor Stage waits for confirmed publication, and uncertain responses reconcile exact marker/summary/signature rather than blindly post again.
`get_handoff` returns the latest successful scoped handoff for the Workflow/PR/head or `null`, including ID, head, summary, time, and `publication_confirmed`.
Historical internal-only handoffs remain readable with `publication_confirmed=false` and are not retroactively described as published.
Reviewer summaries are claims to verify; inline findings use `submit_review.comments` and bodies hold findings without valid diff locations.
