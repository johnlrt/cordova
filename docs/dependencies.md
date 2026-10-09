# Dependency decisions

The Rust node pins direct crate versions in `node/Cargo.toml` and commits `node/Cargo.lock`. The reviewed source commit and release checksums for native llama.cpp binaries still need an M0 entry in `contracts/versions.lock.json`; no binary digest is invented here.

| Crate | Pinned version | Use |
| --- | --- | --- |
| `tokio` | 1.53.1 | Async runtime, process supervision, signals, networking, bounded work and deadlines. |
| `tokio-tungstenite` | 0.30.0 | WebSocket framing and TLS client; compression is not enabled. |
| `reqwest` | 0.12.28 | Enrollment, authentication and HTTPS model downloads; configured SOCKS5h proxy support. |
| `keyring` | 3.6.3 | OS credential store for node, enrollment and proxy credentials. |
| `ed25519-dalek` | 2.2.0 | Signed catalog manifest verification. |
| `hmac` | 0.13.0 | Node authentication challenge proof. |
| `sha2` | 0.11.0 | Artifact, manifest, and hardware-fingerprint hashes. |
| `base64` | 0.22.1 | Catalog keys, signatures, and URL-safe enrollment secrets. |
| `serde` / `serde_json` | 1.0.229 / 1.0.151 | Strict typed protocol and configuration serialization. |
| `clap` | 4.6.1 | CLI parsing and environment-backed options. |
| `chrono` | 0.4.45 | RFC3339 timestamps and conversion to monotonic deadlines. |
| `rand` | 0.9.5 | UUIDs, installation salt, runner API keys, and ephemeral ports. |
| `futures-util` | 0.3.34 | WebSocket sink/stream and local runner SSE processing. |
| `tokio-util` | 0.7.19 | Cancellation tokens for leases and child work. |
| `nix` | 0.30.1 | Unix process-group termination; target-specific to Unix. |
| `fs2` | 0.4.3 | Model-cache free-space checks. |
| `url` | 2.5.8 | Origin, HTTPS, host allowlist, and proxy validation. |
| `hex` | 0.4.3 | Hash and secret encoding. |
| `thiserror` | 2.0.18 | Typed internal errors. |
| `tracing` / `tracing-subscriber` | 0.1.44 / 0.3.23 | Content-free diagnostics using safe error codes. |

The node uses llama.cpp's `llama-server` executable as a separate process. Its release artifact, checksum, license, and exact commit must be verified and recorded before production packaging. The node does not add a Rust FFI binding or a native build dependency for llama.cpp.
