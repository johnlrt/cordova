# M1 — mock-node vertical slice

This change adds a local development path for the first dashboard-backed request flow:

```text
Browser → TLS nginx edge → Go gateway → WSS synthetic node
                              ↓                 ↓
                         Redis outbox → Kafka → account usage projection
```

The frontend displays only completed metadata. Prompts and generated text do not enter Redis or Kafka. The M1 chat route now runs through the M3 coordinator with a zero-priced ledger snapshot; usage counts remain synthetic word counts and charges remain zero. Real llama.cpp inference, node enrollment/routing, authoritative tokenization, real billing, deposits, and payouts remain disabled.

## Included behavior

- Account registration, login, recovery-code rotation, revocable Redis sessions, CSRF checks, auth throttles, and Argon2id password hashes.
- API-key creation with a one-time secret, HMAC verifier indexing, owner-only listing, revocation, scope checks, and a 20-key account cap.
- OpenAI-style model listing and SSE chat through a private TLS WebSocket synthetic node. The gateway's synthetic tokenizer recounts usage; the zero-priced ledger stores request lifecycle metadata and the usage projection stores content-free rows only.
- Redis Streams outbox to Kafka with account partitioning and idempotent metadata projection. Dashboard usage is fetched from the projection; C4 live WebSocket invalidations use a short-lived, one-use, session-bound ticket and contain no content. Owner-scoped node invalidations cover approval, rename, revoke, heartbeat and model-state changes; node list/detail pages refetch authoritative REST snapshots and poll for heartbeat expiry.
- Svelte dashboard, login, recovery and API-guide pages. It refreshes usage after a live invalidation and polls every 30 seconds as a fallback.
- Local nginx TLS, SSE and WebSocket proxying, strict TLS versions, browser security headers, denied public readiness/metrics/payment routes, private Redis/Kafka/node networks, and generated local secrets/certificates.
- Static frontend deep links use the generated `200.html` fallback through nginx and the local Go frontend wrapper, including UUID-addressed node detail routes.
- OpenAPI account/key/usage routes and C3/C4 registries checked in CI without Docker or service credentials.
- Optional C3 enrollment and HMAC challenge/JWT HTTP endpoints. They stay disabled unless both an AES-256 node-secret encryption key and an Ed25519 signing key are configured.

The local mock-node WSS framing remains the default M1 synthetic adapter. Set `CORDOVA_INFERENCE_MODE=c3` to select the C3 router; this requires a signed model catalog, trusted tokenizer, node API keys, and enrolled Rust nodes. The router queries fresh model state and claims only loaded models with a current server-written benchmark profile, using session-fenced Redis slot leases. It orders idle loaded nodes before busy loaded nodes and applies rank/first-token-latency ordering within each tier. When route demand finds a loaded model without a current profile, a bounded background scheduler benchmarks one node at a time. For a verified on-disk model with a current capacity profile, the router takes a session-scoped Redis lock, loads only the signed catalog artifact within the node's 10-second load deadline, checks the returned artifact/tokenizer/template hashes, then claims an inference lease for the triggering request. If no current profile exists, a bounded background scheduler loads the on-disk model and schedules the required benchmark. For an absent model, it requires a fresh disk-space report, rechecks the session and model state under the same Redis lock, and runs the node's signed-catalog download-and-load path in the background; a shared Redis permit caps those downloads at two across gateway instances, and the request that triggered warmup is never served by that job. The lock is renewed during background preparation and released before benchmarking. Loading and benchmarking block inference leases for that model. The benchmark uses synthetic nonce prompts, separate session-bound leases, exact server-side output token counts and arrival timing, and a conservative 20% margin below reported memory as a trial ceiling; benchmark success is still required before capacity is recorded. The node-reported peak-memory value never authorizes capacity. Server-side rank projection, benchmark refresh triggers beyond route demand, retry across every C6 tier, production catalog/model qualification, and full M2 qualification remain incomplete. Do not describe the current slice as a qualified production inference service.

To enable the enrollment/auth HTTP routes, provide both `CORDOVA_NODE_SECRET_ENCRYPTION_KEY` and `CORDOVA_NODE_JWT_SIGNING_KEY`, or their `_FILE` variants. Values may be hex or base64; the encryption key is exactly 32 bytes, and the Ed25519 signing value may be a 32-byte seed or canonical 64-byte private key. Keep these outside source control and rotate them using the key-management procedure before production. When either key is missing, `/api/node/*` and owner node-management routes are not registered. Owner approval, link/rename/revoke, and `POST /api/nodes/{id}/models/{model_id}/prepare` are protected by the dashboard session, same-origin check, and CSRF token. Model preparation accepts only a model in the verified signed catalog and schedules work through the C3 router's session-fenced load lock, bounded download permit and benchmark scheduler; it never dispatches an inference request. `/dashboard/nodes` accepts the one-time CLI code and provides paginated owner inventory, status filtering, detail, rename, revoke and a preparation action for model reports. C4 emits account-scoped metadata invalidations for approval, rename, revoke, heartbeats and model-state changes; the node list and detail pages refetch authoritative REST snapshots and poll for heartbeat expiry. Measurements/rank/earnings projections remain later-stage work; unmeasured fields are null. The owner action has a Redis-backed HTTP integration test; the current local Windows run compiled that test but had no Redis service to execute it.

## Start the local stack

From the repository root:

```sh
make bootstrap-dev
make config-check
make up
make smoke
make network-check
make test-edge
```

`make config-check` validates Compose service/image/network boundaries, secret file permissions, certificate chains and hostnames. `make smoke` registers a disposable local account, creates a test key, checks models and SSE completion, then waits for the same usage record in the dashboard projection. `make test-edge` checks TLS headers, deep-link refresh, private-route denial, C4 upgrade, one-use ticket and application heartbeat. Neither smoke script prints account secrets.

`make down` stops services and keeps named data volumes. The stack is for local development only. Container tags use explicit versions, but they are not immutable registry digests; digest and image-signing qualification remain open release work.

## Checks and limits

- `make contracts-check` validates REST route inventory, C3/C4 type registries, tokenizer contracts and positive/negative fixtures.
- CI runs Go formatting, vet, race tests, builds both Go commands, tests and lints the Rust node, checks frontend types/tests/build and all contracts, and audits Go/Rust/npm dependencies. A separate `frontend-m1` job boots the isolated HTTPS Compose stack and exercises registration, a synthetic request, live usage refresh, account isolation, and edge smoke checks through nginx.
- Hosted GitHub Actions run `38266282790` passed all jobs on 2026-10-12: the Redis-backed Go race/integration suite, C3 routing and ranker replay checks, Rust tests/lint/audit, frontend checks, and the isolated `frontend-m1` stack including `make smoke`, live usage refresh, and `make test-edge`. This verifies the mock-node vertical slice only.
- The current Windows checkout lacks Docker, so Compose checks have not run locally. Hosted run `38266282790` covers `make smoke` and `make test-edge`; `make network-check` still needs an execution record.
- M0 Redis/Lua/WAITAOF, Kafka duplication/recovery, tokenizer/model parity, SOCKS5h, and wallet probes remain prerequisites for later milestones. No production secret, model artifact, wallet RPC, or public port is configured by the local stack.
