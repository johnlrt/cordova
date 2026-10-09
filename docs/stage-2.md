# Stage 2 — contract-first server foundation

Date: 2026-10-09. This is the first backend increment after the stage-1 mock API. It starts PLAN-back B0 by publishing contracts for the current HTTP surface, the planned node/dashboard WebSockets, and the private tokenizer boundary.

## Contract foundation increment

- `contracts/openapi.yaml` describes the implemented `/healthz`, `/v1/models` and `/v1/chat/completions` routes, including the accepted C2 generation controls and synthetic mock usage.
- `contracts/ws/v1.schema.json` records all 27 node and 8 dashboard live message variants from C3/C4, direction groups, frame bounds, and the initial-session ID rule. The check gate verifies the registries and representative valid and invalid frames.
- `contracts/internal/tokenizer.schema.json` records the C9 request/response shapes, safe failures, helper-only auth boundary and operational limits.
- `contracts/fixtures/` contains accepted and rejected examples. Run `sh scripts/contracts-check.sh` to check JSON syntax, local references, route/message inventory and the supported schema subset against those fixtures.
- The contract checker uses only Python's standard library. `openapi.yaml` is JSON-formatted YAML so this first gate can parse it without adding a YAML dependency.

## Go request increment

The gateway now accepts the C2 `temperature`, `top_p`, `stop`, `seed`, `n=1` and `user` fields. It validates their ranges and types, normalizes scalar `stop` into a list, applies the documented sampling defaults, and passes generation controls through `inference.Provider`. The demo `inference.Mock` still emits fixed synthetic text and word counts; it does not apply sampling, stop sequences, or model tokenization. The `user` field is discarded and is not sent to the provider.

## Runtime status

The gateway still uses `inference.Mock` and reports synthetic word counts. Context-window enforcement, tokenizer helper, Rust node, WebSocket sessions, account authentication, Redis/Kafka, real tokens and billing are not implemented by this increment. The OpenAPI file describes the live HTTP surface and marks behavior that remains mock-only; it does not claim that the full C1–C4/C9 plan has been delivered.

## Remaining B0 work

1. Expand OpenAPI to all C1/C2 account, key, node, billing, usage and live-session DTOs and operations.
2. Generate and check Go, Rust and TypeScript bindings from the schemas; expand protocol fixtures to cover every message variant and its rejection cases.
3. Run and record the external Redis/Lua/WAITAOF, Kafka outbox, model/tokenizer parity, SOCKS5h and stagenet wallet spikes described by PLAN-back B0. This increment does not assert those integrations were available or verified.

The next application milestone remains B1, followed by B2's mock-node vertical slice. Do not admit a real model until the artifact, license, runner and tokenizer parity gates pass.
