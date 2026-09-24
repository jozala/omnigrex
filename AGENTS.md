# AGENTS.md — Working on Omnigrex

Read the minimum repository context needed for the change.

## Read first

- `README.md` — development commands, Compose secrets, and test entry points.
- `CONTEXT.md` — domain terminology (Work Item, Workflow, Change Proposal, Role, Agent Session, and related terms). Use these terms.
- `docs/adr/` — architecture decisions behind the current design.
- `docs/operator-guide.md` — deployment, GitHub Apps, secrets, backup/restore, and recovery procedures. Link to it; do not copy its procedures here.

## Repository map

- `cmd/omnigrex/` — process entry point.
- `internal/` — Go implementation: `config`, `workflow`/`workflowaction`, `store`, `github`, `runtime/`, `mcp`, `workspace`, `agentturn`, `doctor`, `deployment`, plus supporting packages.
- `internal/store/migrations/` — forward-only embedded PostgreSQL migrations.
- `agent/opencode/` — common non-root OpenCode Runtime Process image.
- `.omnigrex/team/` — repository Agent Profiles (one per Workflow Role).
- `compose.yaml`, `Dockerfile`, `mise.toml`, `.env.example` — stack, images, pinned toolchain, and local configuration shape.

## How to work

- When an Issue is assigned, read it and the relevant code before changing anything; otherwise follow the task's stated requirements. Keep changes focused accordingly.
- Follow `CONTEXT.md` terms and the ADRs that apply; do not reintroduce rejected alternatives.
- Do not modify `docs/first-iteration.md`; it is an accepted specification.
- Capture implementation failures and regression behavior in tests when possible, rather than adding incident notes or implementation details to accepted specifications.
- When tests are insufficient to explain an operational issue, use focused documentation under a dedicated `docs/` directory with one file per area.

## Verification

- See `README.md` and `mise.toml` for exact commands. The normal local gate is:

  ```sh
  mise install
  mise run check
  ```

- Run `mise run check` for every code change, plus the narrowest applicable tests. Run `mise run test-integration` when Docker-backed behavior is affected; it needs Docker. `mise run test-compose` deliberately recreates stable local networks and volumes, so run it only when those resources can be removed.

## Secrets and safety

- Keep credentials and deployment secrets (`.env`, `secrets/`, private keys, webhook secrets, provider credentials, compatibility artifacts containing deployment metadata, backups) out of commits and documentation.
- Link to `README.md` and `docs/operator-guide.md` for secret and deployment procedures instead of duplicating values or steps.

## Omnigrex-run versus external agents

- Agents run by Omnigrex must also follow their `.omnigrex/team/` Role instructions and use Omnigrex MCP tools for all GitHub reads and mutations.
- This file stays applicable outside Omnigrex: external agents follow the same read-first, focused-change, and verification discipline with their own GitHub tooling, without duplicating Role profiles or the operator guide.
