# Rust inference node

The `node/` crate provides the `cordova-node` CLI and a Rust client for the C3 outbound WebSocket protocol. It runs `llama-server` as a child process; it does not link llama.cpp through FFI. The node verifies signed catalog manifests and model hashes before loading a model.

## Build

Install the Rust toolchain pinned in `node/rust-toolchain.toml`, then run:

```sh
cd node
cargo build --release
```

Install a `llama-server` executable built from the full 40-character commit selected by the signed model catalog. Set the executable path and trust settings before login:

```sh
cargo run -- config set llama-server /absolute/path/to/llama-server
cargo run -- config set catalog-public-key BASE64_ED25519_PUBLIC_KEY
cargo run -- config set allowed-model-hosts models.example.org
```

The catalog key has no default. The node fails closed until an operator configures it. The executable can also be found through `PATH`; its `--version` output must report at least a 9-character commit prefix matching the catalog commit. The project does not yet package a signed llama.cpp binary or verify its file checksum.

## Commands

```sh
cargo run -- login --server https://api.example.org
cargo run -- login --server https://api.example.org --proxy socks5h://127.0.0.1:9050
cargo run -- run --foreground
cargo run -- status
cargo run -- config show
cargo run -- config path
cargo run -- logout
```

`login` prints a verification link to `/dashboard/nodes` and a user code, then waits for owner approval. The page requires an authenticated account and asks the owner to confirm the code shown by the CLI. It stores node and proxy secrets in the operating system credential store. Linux and macOS install a user service after enrollment when systemd or launchd is available. Windows currently runs in the foreground. `config show` omits the installation salt, public key value, and credential-store contents.

The node accepts runtime settings through `run` flags or the `CORDOVA_NODE_*` environment variables. `--server` is only for first enrollment. Proxy username and password can be stored with `config set proxy-username ...` and `config set proxy-password ...`; they never belong in the proxy URL.

## Current integration boundary

The Go server has opt-in enrollment and HMAC challenge/JWT HTTP endpoints. They are enabled only when both `CORDOVA_NODE_SECRET_ENCRYPTION_KEY` and `CORDOVA_NODE_JWT_SIGNING_KEY` (or their `_FILE` variants) are set. The keys accept hex or base64; use a 32-byte AES key and a 32-byte Ed25519 seed or canonical 64-byte private key. `/api/node/ws` accepts the C3 hello, empty catalog, heartbeat and goodbye flow with reconnect fencing. The dashboard can approve a one-time code and list, inspect, rename or revoke the signed-in owner's nodes. Hardware is explicitly shown as reported; unmeasured rank, capacity, throughput and earnings remain unavailable. A signed catalog, model commands, Redis slot leases, and gateway inference dispatch are still missing, so the CLI cannot serve requests through this gateway and no model can be admitted or run here.

The node currently runs one inference slot. It accepts one-slot synthetic benchmark challenges; it reports zero verified capacity and zero peak VRAM rather than inferring those values from hardware. Multi-slot calibration, server-side enrollment/WebSocket handling, generated Rust bindings from `contracts/ws/v1.schema.json`, and cross-platform transport smoke tests remain open work. The current source keeps strict typed protocol DTOs in one crate.

Run the verified local checks from `node/`:

```sh
cargo fmt --all
cargo test --workspace
cargo clippy --all-targets --all-features -- -D warnings
```
