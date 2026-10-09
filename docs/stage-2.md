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
- Optional C3 enrollment and HMAC challenge/JWT HTTP endpoints. They stay disabled unless both an AES-256 node-secret encryption key and an Ed25519 signing key are configured.

The local mock-node WSS framing is an M1-only synthetic adapter. The Go server supports optional enrollment, access-token issuance, `/api/node/ws`, reconnect fencing, an empty C3 catalog snapshot, heartbeat persistence, and a bounded request-ID multiplexer for validated C3 model/inference replies. The M1 coordinator still does not dispatch inference over C3, serve signed model manifests, or allocate Redis slot leases. The Rust CLI can establish a basic session, but cannot load a server-provided model or serve requests through this gateway. This remains an explicit B2/M2 integration gap; do not describe the current slice as real inference or as a qualified Rust-node connection.

To enable the enrollment/auth HTTP routes, provide both `CORDOVA_NODE_SECRET_ENCRYPTION_KEY` and `CORDOVA_NODE_JWT_SIGNING_KEY`, or their `_FILE` variants. Values may be hex or base64; the encryption key is exactly 32 bytes, and the Ed25519 signing value may be a 32-byte seed or canonical 64-byte private key. Keep these outside source control and rotate them using the key-management procedure before production. When either key is missing, `/api/node/*` is not registered. Owner approval is protected by the dashboard session, same-origin check, and CSRF token; an approval UI has not been added yet.

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
- `make smoke`, `make network-check`, and `make test-edge` require the Compose stack and have not been run unless their output is recorded for this checkout.
- M0 Redis/Lua/WAITAOF, Kafka duplication/recovery, tokenizer/model parity, SOCKS5h, and wallet probes remain prerequisites for later milestones. No production secret, model artifact, wallet RPC, or public port is configured by the local stack.
