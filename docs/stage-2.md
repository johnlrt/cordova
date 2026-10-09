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
- Redis Streams outbox to Kafka with account partitioning and idempotent metadata projection. Dashboard usage is fetched from the projection; C4 live WebSocket invalidations use a short-lived, one-use, session-bound ticket and contain no content.
- Svelte dashboard, login, recovery and API-guide pages. It refreshes usage after a live invalidation and polls every 30 seconds as a fallback.
- Local nginx TLS, SSE and WebSocket proxying, strict TLS versions, browser security headers, denied public readiness/metrics/payment routes, private Redis/Kafka/node networks, and generated local secrets/certificates.
- OpenAPI account/key/usage routes and C3/C4 registries checked in CI without Docker or service credentials.

The local mock-node WSS framing is an M1-only synthetic adapter. It is not yet the full C3 Rust-node session, signed-token, lease, and token-ID protocol. The Rust CLI and llama.cpp subprocess live in `node/`, but are not connected to this M1 gateway. This is an explicit remaining B2 integration gap; do not describe the current slice as real inference or as a qualified Rust-node connection.

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
- CI runs Go formatting, vet, race tests, builds both Go commands, tests and lints the Rust node, checks frontend types/tests/build and all contracts, and audits Go/Rust/npm dependencies.
- `make smoke`, `make network-check`, and `make test-edge` require the Compose stack and have not been run unless their output is recorded for this checkout.
- M0 Redis/Lua/WAITAOF, Kafka duplication/recovery, tokenizer/model parity, SOCKS5h, and wallet probes remain prerequisites for later milestones. No production secret, model artifact, wallet RPC, or public port is configured by the local stack.
