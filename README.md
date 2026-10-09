# Cordova

Cordova is a private mesh inference project. The repository now has the first complete **M1 mock-node slice**: HTTPS edge → Go gateway → private WSS mock node → SSE, with account-scoped metadata persisted through Redis and Kafka into the dashboard.

M1 uses synthetic text and word counts. It creates no real model tokens, charges, deposits, or payouts. The Rust `cordova-node` CLI and llama.cpp runner are in the repository, but real-node enrollment, routing, and inference are later milestones and are not connected to the M1 gateway.

## Run M1 locally

Requirements: Docker Compose v2, OpenSSL, Python 3, and `make`.

```sh
make bootstrap-dev
make config-check
make up
make smoke
make network-check
```

The browser edge is [https://localhost:8443](https://localhost:8443). Trust `.local/dev-ca.crt` in your local browser or OS before using the dashboard. The generated private keys and application secrets stay under `.local/`, which is ignored by Git and excluded from Docker build contexts.

Register an account, save the one-time recovery codes, and create a test API key from the dashboard. The key is shown once. The API guide is available at `/docs/api`.

`make down` stops the containers but keeps the local Redis and Kafka volumes. The stack publishes only `127.0.0.1:8443`; Redis, Kafka, the Go gateway, and the mock WSS node stay on private Compose networks. No wallet or Docker socket is mounted.

## Checks

```sh
make contracts-check
make config-check
make test-protocol
cd frontend && npm ci && npm run check && npm run test && npm run build
cd ../server && go fmt ./... && go test -race -count=1 ./... && go vet ./... && go build ./cmd/gateway ./cmd/mock-node
```

After `make up`, run `make smoke`, `make network-check`, and `make test-edge` for the local Redis/Kafka, TLS edge, SSE, account projection, and C4 WebSocket path. CI runs Go, frontend, and contract checks without Docker or service credentials.

The local M1 stack is development-only. Image tags are version-pinned; immutable registry digests and external M0 probes remain release gates. See [M1 implementation notes](docs/stage-2.md) for the remaining integration gaps.

## Stage 1 fallback

The original standalone mock HTTP/SSE server remains available for the smaller Stage 1 smoke test. Read [docs/stage-1.md](docs/stage-1.md). It does not include account sessions, the dashboard, Redis, Kafka, or the WSS mock-node path.

## Project plans

- [Shared architecture and contracts](PLAN.md)
- [Backend and Rust node work](PLAN-back.md)
- [Frontend work](PLAN-front.md)
- [Infrastructure and release work](PLAN-infra.md)
- [M1 implementation notes](docs/stage-2.md)
