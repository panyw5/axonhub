# Devin Deployment: 2026-11-11

## Build And Runtime

- Built frontend with `pnpm install --ignore-workspace --frozen-lockfile` and
  `./node_modules/.bin/vite build`, then replaced embedded static assets.
- Built backend with `-buildvcs=true`, `-tags=nomsgpack`, stripped symbols and
  linker-injected build information.
- Version: `v1.0.0-beta10+git.83542a5c.devin`.
- Base revision: `83542a5c946268d62a914f71c202452c28d376b4` with uncommitted
  Devin implementation (`vcs.modified=true`).
- Build time: `2026-11-11T14:56:25Z`.
- Active executable: `/Users/lelouch/api/axonhub/axonhub`.
- Active listener at verification: PID `558`, port `8090`.
- Restarted only tmux pane `%0` / `api-servers:0.0`, using the same working
  directory and repository-local executable as `~/servers.sh`.
- Did not modify `servers.sh`, restart the entire session, or replace the unused
  `/usr/local/bin/axonhub` installation.

## Backup Scope

- Per user clarification, request history was excluded from backups. No database
  history was deleted.
- Persistent backup directory:
  `/Users/lelouch/api/axonhub/.agent/backups/devin-20261111.backup/`.
- `configuration.dump`: 818387 bytes, PostgreSQL custom-format archive containing
  database schema and 19 configuration-related tables' data.
- Excluded table data: `requests`, `request_executions`, `usage_logs`, `threads`,
  `traces`, `channel_probes`, `provider_quota_status`.
- Also retained `config.yml`, `servers.sh`, and `axonhub.previous`.
- Backup directory is mode 0700; dump and runtime configuration are mode 0600.
  Git ignores the `.backup` directory.
- Cancelled the initial full database dump and removed its incomplete archive.
- Do not restore this archive with `--clean` over a live database: it contains
  schema for excluded history tables too. Restore to a temporary database or
  restore only specifically selected configuration data after reviewing changes.

## Verification

- Health endpoint returned `healthy` and the new Devin build version.
- Live frontend entry: `/assets/index-BlzTz_8E.js`.
- Live homepage and JavaScript SHA-256 match the new frontend build.
- `~/servers.sh status`: AxonHub 8090, Aether 8084 and Cline 3123 all HTTP 200;
  AIStudio remains disabled as before.
- Frontend build emitted only the existing non-fatal large-chunk warning.

## SHA-256

- Backend: `0d8b6cae8b061850feeaf120463c54d935519f0705e96f5c048c04428fcb010f`.
- Frontend HTML: `0b51868e1bc46ea9c5d5b187490490b0807d3b7eda441ef44c78f3961bae7369`.
- Live/frontend entry JS: `7b44cfe22e7e9eee6e73d0d32f1588f1f7b0634fbe351bb64868e2f11c30b75b`.
