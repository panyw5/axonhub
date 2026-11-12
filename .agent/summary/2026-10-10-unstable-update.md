# Unstable update - 2026-10-10

- Upstream: https://github.com/looplj/axonhub.git, branch unstable.
- Upstream commit: b6d184a8 (2026-10-10 13:18:55 +08:00).
- Active branch: codex/update-unstable-20261010, tracking origin/unstable.
- Merge commit: 9088ea95. Parents: 3af4bf80 and b6d184a8.
- Original branch v1.0.0-beta10 remains unchanged at 3af4bf80.
- Backup branch: codex/backup-before-unstable-20261010.
- Original uncommitted changes remain backed up in the stash named
  backup-before-unstable-20261010 local changes (stash@{0} at completion).

## Preservation and conflicts

All original local commits were retained without rewriting history. All five
original modified files were restored as unstaged changes, with the same
per-file insertion/deletion counts as before the merge.

The Codex default model list conflict was resolved with the complete upstream
list, which includes all locally added GPT-6 model IDs and codex-auto-review.

The model fetcher stash conflict was resolved by retaining upstream Codex
catalog fetching/fallback helpers together with local Command Code endpoint
selection and sanitized error logging.

Local Command Code native protocol support, response reasoning customizations,
deployment workflow, frontend checksum logging, and internal/build/VERSION
(v1.0.0-beta10) were preserved. The version file is not an indication of the
upstream commit now merged; use the Git commit IDs above for identification.

No AxonClaw code was introduced. No push, lint, build, deployment, service
restart, or stash deletion was performed.

## Verification

- Frontend: node --test 'src/**/*.test.mjs': 240 passed, zero failed.
- llm module: go test ./httpclient ./transformer/commandcode
  ./transformer/openai/codex ./transformer/openai/responses ./transformer/shared:
  all five packages passed.
- Root module: go test -tags=nomsgpack ./internal/server/biz
  ./internal/server/orchestrator: both packages passed.
- No unmerged index entries remain.
- git diff --check for restored local changes passed.
- All origin/unstable commits are reachable from HEAD.

Upstream itself contains two trailing-blank-line warnings in
channel-override-template-edit-dialog.tsx and override-operations.ts; these were
left unchanged rather than mixing unrelated formatting into the merge.

## Build and deployment preparation

Frontend and backend production builds subsequently completed successfully:

- pnpm install --ignore-workspace --frozen-lockfile.
- frontend/node_modules/.bin/vite build (only large-chunk warnings).
- Replaced embedded frontend assets with frontend/dist output.
- go build -ldflags '-s -w' -tags=nomsgpack -o axonhub ./cmd/axonhub.
- Executable validation: Mach-O arm64; --version ran successfully.
- Embedded index SHA-256:
  7a7a3c0f7ffe7924e78b32364098f33db74598fc3fa5cd5b7d4ae694de32fdf8.
- Frontend entry: /assets/index-BE_ES65p.js.
- New executable SHA-256:
  51543cfbbf5755f9b00222fa33ce220234108b2885188b9c4df916f1c6347115.

User confirmed deployment only when no requests are pending or processing.
Read-only PostgreSQL checks showed active requests, so no shutdown was attempted.
The PID file was stale: it contained 46274, but the live port 8090 listener was
PID 8757, running /usr/local/bin/axonhub. Revalidate the listener before any stop.

The previous installed executable was backed up to
dumps/2026-10-10-unstable/axonhub.previous. Its SHA-256 is
16f3ee8ae77cec6d2ed816ee8510af23334f3a6b557025fab89241d8af447d75.

Deployment remains pending administrator authorization: sudo -n required a
password, and the macOS authorization dialog was canceled. The system binary
was not replaced, and the live service was not stopped or restarted. No database
backup was created; an initial pg_dump invocation rejected the unsupported -X
option before connecting. Use pg_dump -w -Fc without -X for a future backup.

## Authorization retry

The user subsequently authorized a fresh administrator request. Authorization
succeeded, and the new executable was staged at
/usr/local/bin/axonhub.next-20261010 without replacing the running executable.
Its SHA-256 matches the new executable recorded above.

A complete PostgreSQL custom-format backup was successfully created at
dumps/2026-10-10-unstable/database.dump and its archive listing was validated.
File permissions are 0600. The dumps directory is ignored by Git.

Deployment is waiting for two consecutive checks with zero pending/processing
records across both requests and request_executions. No forced termination is
allowed while requests are active.

The foreground idle wait observed ongoing traffic and was stopped without
touching the service. A proposed five-minute deployment heartbeat was rejected
by system review because future privileged deployment/restarts require explicit
authorization for that persistent automation. No heartbeat was created. Wait
for the user's explicit approval before retrying; do not replace it with an
indirect scheduler or background deployment script.

## Deployment completed

After the user explicitly requested deployment now, a bounded foreground
deployment was performed. No recurring or background automation was created.

The initial helper invocation was rejected because it assigned common process
environment variables. Those assignments were removed. The reviewed helper
instead uses sudo -H -u lelouch to run the repository stop/start scripts as the
original user, while only the installed executable replacement runs privileged.

Two consecutive idle checks and a final immediate check returned zero active
records before shutdown. The verified service PID 8757 was written to the stale
PID file, gracefully stopped, and replaced with the previously validated build.
The replacement service runs as lelouch with PID 27206; its PID file matches.

Verification on 2026-10-10 at 14:19:54 +08:00:

- /health returned healthy.
- Installed executable SHA-256 matches the build recorded above.
- Served homepage SHA-256 matches frontend/dist/index.html.
- Homepage references /assets/index-BE_ES65p.js, which returns HTTP 200 with
  JavaScript content type.
- Reported version is v1.0.0-beta10 because the local version label was
  intentionally preserved; the executable contains unstable b6d184a8 plus local
  customizations.
- Old executable and database backup remain in dumps/2026-10-10-unstable/.

No database restore, forced termination, code push, or modification of the five
original uncommitted changes was performed.
