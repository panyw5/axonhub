# Aether Free Account Migration

Date: 2026-10-21 (Asia/Shanghai).

## Scope and Result

- Migrated 10 additional accounts, excluding the earlier trial channel 819.
- Required an active Free account with a numeric Gmail alias matching `^[^@\s]+\+\d+@(gmail\.com|googlemail\.com)$` (case-insensitive).
- Rechecked the source plan and exported email for every account. Rejected conflicting plan metadata when present; some exports omit the plan field.
- Required ordinary OAuth credentials with access token, refresh token and client ID. No credentials are stored in this report.
- Every target was initially disabled. Model discovery and inference succeeded before disabling its source and enabling its target.
- All 10 targets are enabled. Their source records were retained and disabled.
- Source audit: 83 records retained, active count changed from 82 to 72. Exactly the selected 10 status values changed; all unrelated account statuses were unchanged.
- Combined with the earlier trial, 11 accounts have now been migrated.

## Accounts

Target names use `Aether-free-` followed by the email below. Target channel IDs are numeric suffixes of `gid://axonhub/Channel/<ID>`.

| Email | Aether Key ID | AxonHub Channel ID | Inference Latency |
| --- | --- | --- | --- |
| nsasvlop01+50000@gmail.com | da4d7b0f-0a8e-4c05-bff7-1ae6ea4020d5 | 820 | 5.81 s |
| nsasvlop01+400@gmail.com | fb8ac0e3-ad08-404e-82da-96a3d2e1b8dc | 821 | 3.27 s |
| nsasvlop01+300@gmail.com | 7af80e94-9a2a-49fe-91e0-5b4b1f93474c | 822 | 2.88 s |
| nsasvlop01+200@gmail.com | 37867ac2-6cd4-4ee6-9a33-a40020b626ca | 823 | 4.03 s |
| nsasvlop01+100@gmail.com | ef98fcc8-09ad-4ecb-aa09-ec4a72f99464 | 824 | 2.11 s |
| nsasvlop01+40@googlemail.com | a90b5a1b-c9f8-4e7b-b862-2fa1c2b9d044 | 825 | 2.70 s |
| nsasvlop01+30@googlemail.com | e40b4941-af6e-48cf-9d9b-d320e2e672b2 | 826 | 2.35 s |
| sssvlop07+80@googlemail.com | bf639e07-6a39-4649-867a-00ae413d92ab | 827 | 4.43 s |
| sssvlop07+20@googlemail.com | 223c2a5a-ebc2-4dab-85d7-115830666eaf | 828 | 3.42 s |
| sssvlop07+10@gmail.com | 33dec74a-7b5b-4a88-a11f-7363133c2ce7 | 829 | 2.16 s |

## Configuration and Verification

- Source provider: `4b2a835d-3f13-4231-adf9-3b26070a5071` (`codex`).
- Target type: `codex`.
- Target base URL: `https://chatgpt.com/backend-api/codex#`.
- Tags: `aether-migration`, `aether-free-batch-20261021`.
- Current visible models for every migrated account: `gpt-6-luna`, `gpt-5.6-terra`, `gpt-5.6-luna`.
- Default test model: `gpt-6-luna`; actual inference verified on all 10.
- Final readback confirmed saved access tokens, refresh tokens, client IDs, future expiry values and enabled target statuses.
- No forced token rotation was performed for this batch. The earlier trial separately verified AxonHub renewal, token rotation, persistence and post-refresh inference.
- No application code, build, lint or service restart was needed.

## Rollback

Disable the corresponding AxonHub channel before re-enabling its Aether source. If AxonHub has refreshed the account, first synchronize its latest credentials back to Aether; the retained source credentials may be stale. Never enable both copies for automatic renewal simultaneously.
