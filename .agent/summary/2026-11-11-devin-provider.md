# Devin Native Provider

## Implementation

- Added the `devin` channel/provider and native `devin/chat` outbound format.
- Adapted Connect-RPC/Protobuf framing and model mappings from the local
  CLIProxyAPI checkout. The upstream MIT notice is retained in
  `llm/transformer/devin/LICENSE`.
- Inference uses Devin CLI session tokens. The `devin-session-token$` prefix is
  optional in channel credentials. This is not the public Devin API key format.
- The outbound retains credential-free unified JSON for overrides/persistence;
  its executor serializes Protobuf immediately before transport.
- Supports standard function tools, base64 image inputs, reasoning and opaque
  signatures, cache-aware token usage, and OpenAI Chat/Responses/Anthropic clients.
- Added bounded/gzip frame decoding, required EOS trailers, Connect error mapping,
  reversible portable tool IDs, and split UTF-8 handling for text/tool arguments.
- Registered the bundled model catalog, daily/weekly quota checker, quota
  collection toggle, frontend configuration, normalized quota bars and locales.
- Regenerated Ent and GraphQL artifacts with `make generate`.
- HTTP client now respects explicit empty User-Agent headers and offers an
  opt-in `DisableCompression` flag for framed upstream protocols.

## Configuration And Limits

- Select Devin when creating a channel. Default URL: `https://server.codeium.com`.
- Paste a Devin CLI session token into Session Token. Select a model from the
  bundled catalog or configure an explicit model mapping.
- Use one token per channel for quota reporting. Inference can rotate multiple
  tokens, but quota checks reject multi-token channels instead of applying one
  account's quota to every account.
- OAuth login, remote model-catalog refresh, custom/freeform tools, forced tool
  choice, remote image URLs, audio/video/document inputs and compaction are not
  implemented. Unsupported inputs fail explicitly.
- Text/tools after reasoning are buffered until EOS to deliver complete thought
  signatures before switching blocks on downstream block-oriented protocols.
- No real account credentials were used. Live upstream compatibility remains to
  be smoke-tested. No build, lint, deployment or service restart was performed.

## Verification

- `cd llm && go test ./...`: passed.
- `go test ./internal/server/biz/... ./internal/server/orchestrator ./internal/objects`:
  passed after adding Devin to quota collection's supported-provider registry.
- Frontend configuration/protocol/quota regression tests passed.
- Full frontend type checking reports 188 existing errors. A TypeScript compiler
  comparison against HEAD using an in-memory baseline also reports 188 errors
  with no newly introduced diagnostics.
- `git diff --check`: passed.
