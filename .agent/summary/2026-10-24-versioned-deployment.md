# Versioned Deployment: 2026-10-24

## Source and Build

- Branch: `codex/update-unstable-20261010`
- Commit: `83542a5c946268d62a914f71c202452c28d376b4`
- Fork branch checked against `https://github.com/panyw5/axonhub.git`; commit matches.
- Runtime version: `v1.0.0-beta10+git.83542a5c`
- Build time: `2026-10-24T12:50:51Z` (`2026-10-24 20:50:51 Asia/Shanghai`)
- Toolchain/platform: `go1.26.0`, `darwin/arm64`
- Go VCS metadata: `vcs.modified=false`; full revision matches the source commit.
- Build injected the existing `internal/build.Version`, `Commit`, and `BuildTime` variables via linker flags, with `-buildvcs=true`, `-s -w`, and `-tags=nomsgpack`.
- Frontend dependencies installed with `pnpm install --ignore-workspace --frozen-lockfile`, then frontend built with Vite and copied into `internal/server/static/dist` before building the backend.
- Source working tree remained clean after frontend/backend builds. No source edits were needed.

## Active Deployment

- Listener: port `8090`, PID `90661` at verification.
- Executable: `/Users/lelouch/api/axonhub/axonhub`
- Working directory: `/Users/lelouch/api/axonhub`
- tmux pane: `api-servers:0.0`; restarted in its original pane after `deploy/stop.sh` completed.
- The pane has `remain-on-exit` enabled so startup failures remain visible.
- During the initial deployment, `/usr/local/bin/axonhub` was not updated because sudo required a password. It was subsequently updated with administrator authorization; see the system installation section below. The active service still uses the repository-local executable.
- Previous active executable retained at `/Users/lelouch/api/axonhub/axonhub.bak.20261024-pre-versioned-build`.
- `deploy/stop.sh` removed its stale PID file. The active process is managed through the tmux pane, not `deploy/start.sh`.

## Verification

- `GET http://localhost:8090/health` returned `healthy`, the version above, the full commit above, and the build time above.
- Running executable path verified with process open-file information.
- Frontend entry point served by the live homepage: `/assets/index-BE_ES65p.js`.
- SHA-256 of live JavaScript matches the local frontend build.
- Other tmux panes remained running.
- Frontend build completed with a non-fatal large-chunk warning.
- No lint or test suite was run; verification covered builds, runtime metadata, health, and deployed frontend content.

## SHA-256

| Artifact | SHA-256 |
| --- | --- |
| Active backend executable | `9d2e6b12910e193e41707e6db236ff2a84bc101c4945bcfa79f3663c4063d361` |
| Frontend index HTML / embedded index | `7a7a3c0f7ffe7924e78b32364098f33db74598fc3fa5cd5b7d4ae694de32fdf8` |
| Local and live `/assets/index-BE_ES65p.js` | `6be5038ca1821f204fe61f445d28853e5e715a6d6a8f688467871d7925ee5f2f` |

## System Installation Follow-up

- Installed `/usr/local/bin/axonhub` at approximately `2026-10-24 20:56 Asia/Shanghai` using the macOS administrator authorization dialog.
- Ownership and permissions: `root:wheel`, `755`.
- The installed executable reports version `v1.0.0-beta10+git.83542a5c`, commit `83542a5c946268d62a914f71c202452c28d376b4`, and build time `2026-10-24T12:50:51Z`.
- Installed executable SHA-256: `9d2e6b12910e193e41707e6db236ff2a84bc101c4945bcfa79f3663c4063d361`, identical to the repository-local executable.
- Previous system executable retained at `/Users/lelouch/api/axonhub/axonhub.bak.system-20261024-before-update`.
- The existing 8090 service was not restarted; its health endpoint remained healthy and reported the same build.
