# AGENTS.md - Working on Omnigrex

Read the minimum repository context needed for the change.

## Read first

- `README.md` - product overview and audience entry points; `CONTRIBUTING.md` and `docs/contributors/` for development and test commands.
- `docs/README.md` - documentation hub, reader responsibilities, and authoritative guide locations.
- `CONTEXT.md` - domain terminology (Work Item, Workflow, Change Proposal, Role, Agent Session, and related terms).
  Use these terms.
- `docs/adr/` - architecture decisions behind the current design.
- `docs/decisions/README.md` - index of new project decisions and existing ADRs.
- `docs/operators/README.md` - deployment, GitHub Apps, secrets, backup/restore, and recovery procedures. Link to the focused procedure; do not copy it here.
- `docs/repository-owners/README.md` and `docs/users/README.md` - repository setup and everyday GitHub use.

## Repository map

- `cmd/omnigrex/` - process entry point.
- `internal/` - Go implementation: `config`, `workflow`/`workflowaction`, `store`, `github`, `runtime/`, `mcp`, `workspace`, `agentturn`, `doctor`, `deployment`, plus supporting packages.
- `internal/store/migrations/` - forward-only embedded PostgreSQL migrations.
- `agent/pi/` - common non-root Pi Runtime Process image, owned runner, MCP extension, and qualification fixtures.
- `.omnigrex/team/` - repository Agent Profiles (one per Workflow Role).
- `compose.yaml`, `Dockerfile`, `mise.toml`, `.env.example` - stack, images, pinned toolchain, and local configuration shape.

## How to work

- When an Issue is assigned, read it and the relevant code before changing anything; otherwise follow the task's stated requirements. Keep changes focused accordingly.
- Follow `CONTEXT.md` terms and the ADRs that apply; do not reintroduce rejected alternatives.
- Put any implementation plans or designs you prepare in `docs/plans/`.
- When making or reviewing a consequential product, engineering, operations, Workflow, or architectural decision, load the `omnigrex-decision-records` skill and follow its recording guidance.
- Do not modify `docs/plans/first-iteration.md`; it is an accepted specification.
- Capture implementation failures and regression behavior in tests when possible, rather than adding incident notes or implementation details to accepted specifications.
- When tests are insufficient to explain an operational issue, use focused documentation under a dedicated `docs/` directory with one file per area.

## Verification

- See `CONTRIBUTING.md`, `docs/contributors/testing.md`, and `mise.toml` for exact commands. The normal local gate is:

  ```sh
  mise install
  mise run check
  ```

- Run `mise run check` for every code change, plus the narrowest applicable tests.
  Run `mise run test-integration` when Docker-backed behavior is affected; it needs Docker.
  `mise run test-compose` delegates to disposable Pi deployment smoke with isolated fixture resources; inspect the selected task/script before running full-stack gates on a shared host.

## Secrets and safety

- Keep credentials and deployment secrets (`.env`, `secrets/`, private keys, webhook secrets, provider credentials, compatibility artifacts containing deployment metadata, backups) out of commits and documentation.
- Link to the authoritative procedure under `docs/operators/` for secrets and deployment instead of duplicating values or steps.

## Omnigrex-run versus external agents

- Agents run by Omnigrex must also follow their `.omnigrex/team/` Role instructions and use Omnigrex MCP tools for all GitHub reads and mutations.
- This file stays applicable outside Omnigrex: external agents follow the same read-first, focused-change, and verification discipline with their own GitHub tooling, without duplicating Role profiles or the operator guide.
