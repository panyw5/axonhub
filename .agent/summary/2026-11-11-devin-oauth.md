# Devin Account Authorization

## Implementation

- Added native Devin PKCE authorization in AxonHub. It does not execute or depend
  on CLIProxyAPI. The user signs in on Devin's authorization page, then pastes the
  displayed authorization code into the channel dialog.
- Backend endpoints: `POST /admin/devin/oauth/start` and
  `POST /admin/devin/oauth/exchange`.
- PKCE sessions expire after ten minutes, are bound to the authenticated user,
  require channel write permission, and are consumed before token exchange.
- Callback URL input additionally validates OAuth state. Plain authorization
  codes use the headless PKCE flow.
- Token exchange honors the channel proxy selection and has a 30-second timeout.
  Upstream error bodies, codes and verifiers are not returned in errors.
- Responses prohibit caching. The front end automatically fills
  `credentials.apiKeys` with the normalized session token, retaining the existing
  create/edit persistence path and manual token entry as a fallback.
- Added the localized login section between proxy configuration and Base URL.
- Hardened the shared OAuth hook against late results after reset, unmount or a
  new authorization attempt. Used sessions and authorization codes are cleared.

## Verification

- `go test ./internal/server/api ./internal/server ./cmd/axonhub`: passed.
- `cd llm && go test ./transformer/devin ./oauth ./httpclient`: passed.
- Frontend OAuth lifecycle/channel/protocol/quota regression tests: 56 passed.
- TypeScript comparison: HEAD baseline 188 diagnostics, current 188, no new
  diagnostics.
- Frontend and backend production builds passed. Frontend emitted only the
  existing large-chunk warning.
- Native account completion still requires the user's actual Devin authorization;
  automated tests used mocked upstream token exchange, not real credentials.

## Deployment

- Version: `v1.0.0-beta10+git.83542a5c.devin.oauth`.
- Build time: `2026-11-11T15:50:08Z`.
- Backend SHA-256:
  `ded720ebb3934bf5d0678db26badcfa5c16e9db8b468787623d8c66f6923260d`.
- Only AxonHub's tmux pane `%0` was restarted; deployment still uses
  `/Users/lelouch/api/axonhub/axonhub` as configured in `~/servers.sh`.
- Previous binary:
  `.agent/backups/devin-oauth-20261111.backup/axonhub.previous`.
- Reused the existing configuration-only database backup. No request-history
  backup or deletion was performed.
- Health endpoint reports healthy and the new OAuth build version.
- Live HTML matches the new build: `/assets/index-Dj3RbpL1.js`.
- Live channel component `/assets/channels-dialogs-BClyyyaz.js` matches the build
  and contains the login UI, both native authorization endpoints and credential
  write path. SHA-256:
  `b4dd1835964884e6b2aed0d2694199b4a24284617f9e1fe4472dfc3db0d893ab`.
